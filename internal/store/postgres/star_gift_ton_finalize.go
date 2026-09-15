package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"telesrv/internal/domain"
	storepkg "telesrv/internal/store"
	"telesrv/internal/store/postgres/sqlcgen"
)

// StarGiftTONFinalizerStore is the Core-owned half of TON export. It can
// mutate Telegram gift/profile state; the chain worker cannot construct it.
type StarGiftTONFinalizerStore struct {
	db        sqlcgen.DBTX
	lifecycle *StarGiftLifecycleStore
	ton       *StarGiftTONStore
}

var _ storepkg.StarGiftTONFinalizerStore = (*StarGiftTONFinalizerStore)(nil)
var _ storepkg.StarGiftTONClaimStore = (*StarGiftTONFinalizerStore)(nil)

func NewStarGiftTONFinalizerStore(db sqlcgen.DBTX, lifecycle *StarGiftLifecycleStore) *StarGiftTONFinalizerStore {
	return &StarGiftTONFinalizerStore{db: db, lifecycle: lifecycle, ton: NewStarGiftTONStore(db)}
}

func (s *StarGiftTONFinalizerStore) ClaimTONCoreJobs(ctx context.Context, workerID string, now, leaseSeconds, limit int) ([]domain.StarGiftTONJob, error) {
	workerID = strings.TrimSpace(workerID)
	if s == nil || s.db == nil || s.lifecycle == nil || s.lifecycle.messages == nil || workerID == "" || len(workerID) > 128 ||
		now <= 0 || leaseSeconds <= 0 || leaseSeconds > 3600 || limit <= 0 || limit > 100 {
		return nil, domain.ErrStarGiftTONExportInvalid
	}
	rows, err := s.db.Query(ctx, `WITH candidates AS (
 SELECT j.id FROM star_gift_ton_jobs j
 JOIN star_gift_ton_exports e ON e.id=j.export_id
 WHERE j.attempts<j.max_attempts
   AND ((j.status='pending' AND j.available_at<=$1) OR (j.status='leased' AND j.lease_expires_at<=$1))
   AND ((j.kind='finalize' AND e.status='confirmed') OR
        (j.kind='sync_owner' AND e.status='finalized'))
 ORDER BY j.available_at,j.id
 FOR UPDATE OF j SKIP LOCKED LIMIT $2
)
UPDATE star_gift_ton_jobs j SET status='leased',lease_owner=$3,lease_expires_at=$1+$4,
 fencing_token=j.fencing_token+1,attempts=j.attempts+1,updated_at=now()
FROM candidates c WHERE j.id=c.id RETURNING j.id`, now, limit, workerID, leaseSeconds)
	if err != nil {
		return nil, fmt.Errorf("claim TON Core jobs: %w", err)
	}
	ids := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	jobs := make([]domain.StarGiftTONJob, 0, len(ids))
	for _, id := range ids {
		job, found, err := s.ton.tonJobByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if found {
			jobs = append(jobs, job)
		}
	}
	return jobs, nil
}

func (s *StarGiftTONFinalizerStore) RenewTONCoreJob(ctx context.Context, jobID int64, workerID string, fencingToken int64, now, leaseSeconds int) error {
	if s == nil || s.ton == nil {
		return domain.ErrStarGiftTONLeaseLost
	}
	return s.ton.RenewTONJob(ctx, jobID, workerID, fencingToken, now, leaseSeconds)
}

func (s *StarGiftTONFinalizerStore) FinalizeTONExport(ctx context.Context, finalization domain.StarGiftTONFinalization) (domain.StarGiftTONFinalizationResult, error) {
	if s == nil || s.db == nil || s.lifecycle == nil || s.lifecycle.messages == nil || !finalization.Valid() {
		return domain.StarGiftTONFinalizationResult{}, domain.ErrStarGiftTONExportInvalid
	}
	lockScope, err := loadTONFinalizeLockScope(ctx, s.db, finalization.JobID)
	if err != nil {
		return domain.StarGiftTONFinalizationResult{}, err
	}
	var out domain.StarGiftTONFinalizationResult
	err = withTx(ctx, s.db, "finalize TON star gift export", func(tx pgx.Tx) error {
		if err := lockUserStarGiftProjectionScope(ctx, tx, lockScope.Projection); err != nil {
			if errors.Is(err, errUserStarGiftProjectionLockScopeChanged) {
				return domain.ErrStarGiftTONExportStateConflict
			}
			return err
		}
		if err := lockStarGiftProfiles(ctx, tx, domain.Peer{Type: domain.PeerTypeUser, ID: lockScope.OwnerUserID}); err != nil {
			return err
		}
		job, export, err := lockTONJobAndExport(ctx, tx, finalization.JobID)
		if err != nil {
			return err
		}
		if job.Kind != domain.StarGiftTONJobFinalize || job.Status != domain.StarGiftTONJobLeased ||
			job.LeaseOwner != strings.TrimSpace(finalization.WorkerID) || job.FencingToken != finalization.FencingToken ||
			job.LeaseExpiresAt <= finalization.FinalizedAt {
			return domain.ErrStarGiftTONLeaseLost
		}
		if export.Status != domain.StarGiftTONExportConfirmed {
			return domain.ErrStarGiftTONExportStateConflict
		}
		if export.ID != lockScope.ExportID || export.UniqueGiftID != lockScope.UniqueGiftID ||
			export.OwnerUserID != lockScope.OwnerUserID {
			return domain.ErrStarGiftTONExportStateConflict
		}
		asset, found, err := tonAssetByExportIDForUpdate(ctx, tx, export.ID)
		if err != nil || !found || asset.ConfirmedAt == 0 || asset.OwnerAddress != export.WalletAddress ||
			asset.ItemAddress != export.ExpectedItemAddress || asset.CollectionAddress != export.CollectionAddress ||
			asset.Network != export.Network || !bytes.Equal(asset.MetadataHash, export.MetadataHash) {
			return domain.ErrStarGiftTONExportStateConflict
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM unique_star_gifts WHERE id=$1 FOR UPDATE`, export.UniqueGiftID); err != nil {
			return err
		}
		saved, found, err := lockSavedStarGiftByUniqueID(ctx, tx, export.UniqueGiftID)
		if err != nil || !found || saved.ID != lockScope.SavedGiftID ||
			saved.Owner != (domain.Peer{Type: domain.PeerTypeUser, ID: export.OwnerUserID}) ||
			!saved.LifecycleStatus.Live() || !lockScope.Projection.matches(saved) {
			return domain.ErrStarGiftTONExportStateConflict
		}
		unique, found, err := NewStarGiftStore(tx).UniqueByID(ctx, export.UniqueGiftID)
		if err != nil || !found || unique.Burned || !unique.ExternalizationPending || unique.OwnerAddress != "" || unique.Owner != saved.Owner {
			return domain.ErrStarGiftTONExportStateConflict
		}
		if err := ensureNoStarGiftMarketConflict(ctx, tx, unique.ID); err != nil {
			return domain.ErrStarGiftTONExportStateConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE peer_star_gifts SET lifecycle_status='exported',unsaved=true,pinned_order=0,
 transfer_stars=0,can_export_at=0,can_transfer_at=0,can_resell_at=0,drop_original_details_stars=0,can_craft_at=0
 WHERE id=$1`, saved.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE star_gift_ton_exports SET status='finalized',finalized_at=$2,
 version=version+1,last_error='',updated_at=now() WHERE id=$1`, export.ID, finalization.FinalizedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE unique_star_gifts SET owner_peer_type=NULL,owner_peer_id=NULL,
 owner_address=$2,gift_address=$3,host_peer_type='user',host_peer_id=$4,externalization_pending=false,
 craft_chance_permille=0,updated_at=now() WHERE id=$1`, unique.ID, asset.OwnerAddress, asset.ItemAddress, export.OwnerUserID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO star_gift_ton_profile_links(
 export_id,unique_gift_id,host_user_id,unsaved,pinned_order,linked_at)
 VALUES($1,$2,$3,$4,$5,$6)`, export.ID, unique.ID, export.OwnerUserID, saved.Unsaved, saved.PinnedOrder, finalization.FinalizedAt); err != nil {
			return err
		}
		unique.Owner = domain.Peer{}
		unique.OwnerAddress = asset.OwnerAddress
		unique.GiftAddress = asset.ItemAddress
		unique.Host = domain.Peer{Type: domain.PeerTypeUser, ID: export.OwnerUserID}
		unique.ExternalizationPending = false
		unique.CraftChancePermille = 0
		unique.ResellAmount = nil
		if _, err := s.lifecycle.retireUserStarGiftMessagesTx(ctx, tx, nil, saved, unique,
			lockScope.Projection, finalization.FinalizedAt); err != nil {
			return err
		}
		if err := updateStarGiftResaleProjection(ctx, tx, unique.GiftID); err != nil {
			return err
		}
		if err := completeTONJob(ctx, tx, job.ID, finalization.FinalizedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO star_gift_ton_jobs(export_id,job_key,kind,available_at,created_at,max_attempts)
 VALUES($1,$2,'reconcile_owner',$3,$3,1000) ON CONFLICT(job_key) DO NOTHING`, export.ID,
			fmt.Sprintf("reconcile-owner:%d", export.ID), finalization.FinalizedAt); err != nil {
			return err
		}
		out.PreviousHost = saved.Owner
		return loadTONFinalizationResult(ctx, tx, export.ID, &out)
	})
	if err != nil {
		return domain.StarGiftTONFinalizationResult{}, err
	}
	return out, nil
}

func (s *StarGiftTONFinalizerStore) SyncTONOwner(ctx context.Context, finalization domain.StarGiftTONFinalization) (domain.StarGiftTONFinalizationResult, error) {
	if s == nil || s.db == nil || !finalization.Valid() {
		return domain.StarGiftTONFinalizationResult{}, domain.ErrStarGiftTONExportInvalid
	}
	lockScope, err := loadTONProfileLockScope(ctx, s.db, finalization.JobID)
	if err != nil {
		return domain.StarGiftTONFinalizationResult{}, err
	}
	var out domain.StarGiftTONFinalizationResult
	err = withTx(ctx, s.db, "sync TON star gift owner", func(tx pgx.Tx) error {
		if lockScope.HostUserID > 0 {
			if err := lockStarGiftProfiles(ctx, tx, domain.Peer{Type: domain.PeerTypeUser, ID: lockScope.HostUserID}); err != nil {
				return err
			}
		}
		job, export, err := lockTONJobAndExport(ctx, tx, finalization.JobID)
		if err != nil {
			return err
		}
		if job.Kind != domain.StarGiftTONJobSyncOwner || job.Status != domain.StarGiftTONJobLeased ||
			job.LeaseOwner != strings.TrimSpace(finalization.WorkerID) || job.FencingToken != finalization.FencingToken ||
			job.LeaseExpiresAt <= finalization.FinalizedAt {
			return domain.ErrStarGiftTONLeaseLost
		}
		if export.Status != domain.StarGiftTONExportFinalized {
			return domain.ErrStarGiftTONExportStateConflict
		}
		if export.ID != lockScope.ExportID || export.UniqueGiftID != lockScope.UniqueGiftID {
			return domain.ErrStarGiftTONExportStateConflict
		}
		asset, found, err := tonAssetByExportIDForUpdate(ctx, tx, export.ID)
		if err != nil || !found || asset.ConfirmedAt == 0 || strings.TrimSpace(asset.OwnerAddress) == "" {
			return domain.ErrStarGiftTONExportStateConflict
		}
		var expectedHostID int64
		err = tx.QueryRow(ctx, `SELECT host_user_id FROM star_gift_ton_profile_links WHERE export_id=$1`, export.ID).Scan(&expectedHostID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if expectedHostID != lockScope.HostUserID {
			return domain.ErrStarGiftTONExportStateConflict
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM unique_star_gifts WHERE id=$1 FOR UPDATE`, export.UniqueGiftID); err != nil {
			return err
		}
		unique, found, err := NewStarGiftStore(tx).UniqueByID(ctx, export.UniqueGiftID)
		if err != nil || !found || unique.GiftAddress != asset.ItemAddress || unique.OwnerAddress == "" {
			return domain.ErrStarGiftTONExportStateConflict
		}
		if unique.OwnerAddress == asset.OwnerAddress {
			if err := completeTONJob(ctx, tx, job.ID, finalization.FinalizedAt); err != nil {
				return err
			}
			return loadTONFinalizationResult(ctx, tx, export.ID, &out)
		}
		var hostUserID int64
		var savedID int64
		err = tx.QueryRow(ctx, `SELECT l.host_user_id,p.id FROM star_gift_ton_profile_links l
 JOIN peer_star_gifts p ON p.unique_gift_id=l.unique_gift_id
 WHERE l.export_id=$1 FOR UPDATE OF l,p`, export.ID).Scan(&hostUserID, &savedID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if hostUserID != expectedHostID {
			return domain.ErrStarGiftTONExportStateConflict
		}
		if hostUserID > 0 {
			out.PreviousHost = domain.Peer{Type: domain.PeerTypeUser, ID: hostUserID}
			if err := removeSavedGiftFromCollections(ctx, tx, out.PreviousHost, savedID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM star_gift_ton_profile_links WHERE export_id=$1`, export.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE unique_star_gifts SET owner_address=$2,host_peer_type=NULL,
 host_peer_id=NULL,updated_at=now() WHERE id=$1`, unique.ID, asset.OwnerAddress); err != nil {
			return err
		}
		if err := completeTONJob(ctx, tx, job.ID, finalization.FinalizedAt); err != nil {
			return err
		}
		return loadTONFinalizationResult(ctx, tx, export.ID, &out)
	})
	if err != nil {
		return domain.StarGiftTONFinalizationResult{}, err
	}
	return out, nil
}

func (s *StarGiftTONFinalizerStore) FailTONCoreJob(ctx context.Context, failure domain.StarGiftTONJobFailure) error {
	if s == nil || s.db == nil || failure.JobID <= 0 || strings.TrimSpace(failure.WorkerID) == "" ||
		failure.FencingToken <= 0 || failure.FailedAt <= 0 || failure.RetryAt <= failure.FailedAt {
		return domain.ErrStarGiftTONLeaseLost
	}
	return withTx(ctx, s.db, "fail TON Core job", func(tx pgx.Tx) error {
		job, export, err := lockTONJobAndExport(ctx, tx, failure.JobID)
		if err != nil {
			return err
		}
		if (job.Kind != domain.StarGiftTONJobFinalize && job.Kind != domain.StarGiftTONJobSyncOwner) ||
			job.Status != domain.StarGiftTONJobLeased || job.LeaseOwner != strings.TrimSpace(failure.WorkerID) ||
			job.FencingToken != failure.FencingToken {
			return domain.ErrStarGiftTONLeaseLost
		}
		terminal := failure.Terminal || job.Attempts >= job.MaxAttempts
		status := "pending"
		if terminal {
			status = "failed"
		}
		if _, err := tx.Exec(ctx, `UPDATE star_gift_ton_jobs SET status=$2,available_at=$3,lease_owner='',
 lease_expires_at=0,last_error=$4,updated_at=now() WHERE id=$1`, job.ID, status, failure.RetryAt,
			boundedTONError(failure.Error)); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE star_gift_ton_exports SET last_error=$2,version=version+1,updated_at=now()
 WHERE id=$1`, export.ID, boundedTONError(failure.Error))
		return err
	})
}

func (s *StarGiftTONFinalizerStore) TONExportByGiftRef(ctx context.Context, ref string) (domain.StarGiftTONExport, domain.StarGiftTONAsset, domain.UniqueStarGift, bool, error) {
	ref = strings.TrimSpace(ref)
	if s == nil || s.db == nil || ref == "" || len(ref) > 255 {
		return domain.StarGiftTONExport{}, domain.StarGiftTONAsset{}, domain.UniqueStarGift{}, false, nil
	}
	var exportID int64
	err := s.db.QueryRow(ctx, `SELECT e.id FROM star_gift_ton_exports e
 JOIN star_gift_ton_assets a ON a.export_id=e.id
 JOIN unique_star_gifts u ON u.id=e.unique_gift_id
 WHERE e.status='finalized' AND NOT u.burned AND (lower(u.slug)=lower($1) OR a.item_address=$1)`, ref).Scan(&exportID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.StarGiftTONExport{}, domain.StarGiftTONAsset{}, domain.UniqueStarGift{}, false, nil
	}
	if err != nil {
		return domain.StarGiftTONExport{}, domain.StarGiftTONAsset{}, domain.UniqueStarGift{}, false, err
	}
	export, found, err := tonExportByPredicate(ctx, s.db, "id=$1", exportID)
	if err != nil || !found {
		return domain.StarGiftTONExport{}, domain.StarGiftTONAsset{}, domain.UniqueStarGift{}, false, err
	}
	asset, found, err := tonAssetByExportID(ctx, s.db, exportID)
	if err != nil || !found {
		return domain.StarGiftTONExport{}, domain.StarGiftTONAsset{}, domain.UniqueStarGift{}, false, err
	}
	gift, found, err := NewStarGiftStore(s.db).UniqueByID(ctx, export.UniqueGiftID)
	return export, asset, gift, found, err
}

func (s *StarGiftTONFinalizerStore) CreateTONClaimChallenge(ctx context.Context, req domain.StarGiftTONChallengeRequest) (domain.StarGiftTONChallenge, error) {
	if req.Purpose != domain.StarGiftTONChallengeClaim || s == nil || s.ton == nil {
		return domain.StarGiftTONChallenge{}, domain.ErrStarGiftTONExportInvalid
	}
	return s.ton.CreateTONProofChallenge(ctx, req)
}

func (s *StarGiftTONFinalizerStore) ScheduleTONOwnerCheck(ctx context.Context, exportID int64, now int) error {
	if s == nil || s.db == nil || exportID <= 0 || now <= 0 {
		return domain.ErrStarGiftTONExportInvalid
	}
	tag, err := s.db.Exec(ctx, `UPDATE star_gift_ton_jobs SET status='pending',available_at=$2,attempts=0,
 lease_owner='',lease_expires_at=0,last_error='',updated_at=now()
 WHERE export_id=$1 AND kind='reconcile_owner' AND status<>'leased'`, exportID, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var leased bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM star_gift_ton_jobs
 WHERE export_id=$1 AND kind='reconcile_owner' AND status='leased')`, exportID).Scan(&leased); err != nil {
			return err
		}
		if !leased {
			return domain.ErrStarGiftTONExportStateConflict
		}
	}
	return nil
}

func (s *StarGiftTONFinalizerStore) CommitTONClaim(ctx context.Context, claim domain.StarGiftTONClaimCommit) (domain.StarGiftTONFinalizationResult, error) {
	if s == nil || s.db == nil || !claim.Valid() {
		return domain.StarGiftTONFinalizationResult{}, domain.ErrStarGiftTONExportInvalid
	}
	lockScope, err := loadTONClaimLockScope(ctx, s.db, claim.ChallengeDigest)
	if err != nil {
		return domain.StarGiftTONFinalizationResult{}, err
	}
	profiles := []domain.Peer{{Type: domain.PeerTypeUser, ID: claim.UserID}}
	if lockScope.HostUserID > 0 {
		profiles = append(profiles, domain.Peer{Type: domain.PeerTypeUser, ID: lockScope.HostUserID})
	}
	var out domain.StarGiftTONFinalizationResult
	err = withTx(ctx, s.db, "claim TON star gift", func(tx pgx.Tx) error {
		if err := lockStarGiftProfiles(ctx, tx, profiles...); err != nil {
			return err
		}
		challenge, found, err := tonProofChallengeByPredicate(ctx, tx, "challenge_digest=$1", claim.ChallengeDigest, true)
		if err != nil || !found || challenge.Purpose != domain.StarGiftTONChallengeClaim || challenge.UserID != claim.UserID ||
			challenge.Status != "pending" || challenge.ExpiresAt <= claim.ClaimedAt || !bytes.Equal(challenge.AuthContextHash, claim.AuthContextHash) {
			return domain.ErrStarGiftTONExportStateConflict
		}
		if challenge.ID != lockScope.ChallengeID || challenge.ExportID != lockScope.ExportID {
			return domain.ErrStarGiftTONExportStateConflict
		}
		export, found, err := tonExportByPredicateForUpdate(ctx, tx, "id=$1", challenge.ExportID)
		if err != nil || !found || export.Status != domain.StarGiftTONExportFinalized || export.Network != challenge.Network {
			return domain.ErrStarGiftTONExportStateConflict
		}
		if export.UniqueGiftID != lockScope.UniqueGiftID {
			return domain.ErrStarGiftTONExportStateConflict
		}
		asset, found, err := tonAssetByExportIDForUpdate(ctx, tx, export.ID)
		if err != nil || !found || asset.OwnerAddress != strings.TrimSpace(claim.WalletAddress) ||
			asset.OwnerObservedAt <= challenge.CreatedAt || asset.OwnerObservedLT == "" {
			return domain.ErrStarGiftTONExportStateConflict
		}
		var previousHostID, savedID int64
		err = tx.QueryRow(ctx, `SELECT l.host_user_id,p.id FROM star_gift_ton_profile_links l
	 JOIN peer_star_gifts p ON p.unique_gift_id=l.unique_gift_id WHERE l.export_id=$1`, export.ID).
			Scan(&previousHostID, &savedID)
		if errors.Is(err, pgx.ErrNoRows) {
			if err := tx.QueryRow(ctx, `SELECT id FROM peer_star_gifts WHERE unique_gift_id=$1`, export.UniqueGiftID).Scan(&savedID); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if previousHostID != lockScope.HostUserID {
			return domain.ErrStarGiftTONExportStateConflict
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM unique_star_gifts WHERE id=$1 FOR UPDATE`, export.UniqueGiftID); err != nil {
			return err
		}
		gift, found, err := NewStarGiftStore(tx).UniqueByID(ctx, export.UniqueGiftID)
		if err != nil || !found || gift.Burned || gift.GiftAddress != asset.ItemAddress || gift.OwnerAddress != asset.OwnerAddress {
			return domain.ErrStarGiftTONExportStateConflict
		}
		var lockedPreviousHostID, lockedSavedID int64
		err = tx.QueryRow(ctx, `SELECT l.host_user_id,p.id FROM star_gift_ton_profile_links l
	 JOIN peer_star_gifts p ON p.unique_gift_id=l.unique_gift_id WHERE l.export_id=$1 FOR UPDATE OF l,p`, export.ID).
			Scan(&lockedPreviousHostID, &lockedSavedID)
		if errors.Is(err, pgx.ErrNoRows) {
			if err := tx.QueryRow(ctx, `SELECT id FROM peer_star_gifts WHERE unique_gift_id=$1 FOR UPDATE`, gift.ID).Scan(&lockedSavedID); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if lockedPreviousHostID != previousHostID || lockedSavedID != savedID {
			return domain.ErrStarGiftTONExportStateConflict
		}
		if previousHostID > 0 && previousHostID != claim.UserID {
			out.PreviousHost = domain.Peer{Type: domain.PeerTypeUser, ID: previousHostID}
			if err := removeSavedGiftFromCollections(ctx, tx, out.PreviousHost, savedID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE unique_star_gifts SET host_peer_type='user',host_peer_id=$2,updated_at=now()
 WHERE id=$1`, gift.ID, claim.UserID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO star_gift_ton_profile_links(
 export_id,unique_gift_id,host_user_id,unsaved,pinned_order,linked_at)
 VALUES($1,$2,$3,false,0,$4)
 ON CONFLICT(export_id) DO UPDATE SET host_user_id=EXCLUDED.host_user_id,unsaved=false,pinned_order=0,
 linked_at=EXCLUDED.linked_at,updated_at=now()`, export.ID, gift.ID, claim.UserID, claim.ClaimedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE star_gift_ton_claims SET status='consumed',wallet_address=$2,
 wallet_state_init_hash=$3,wallet_public_key=$4,proof_hash=$5,verified_at=$6,consumed_at=$6,updated_at=now()
 WHERE id=$1`, challenge.ID, asset.OwnerAddress, claim.StateInitHash, claim.WalletPublicKey, claim.ProofHash, claim.ClaimedAt); err != nil {
			return err
		}
		return loadTONFinalizationResult(ctx, tx, export.ID, &out)
	})
	if err != nil {
		return domain.StarGiftTONFinalizationResult{}, err
	}
	return out, nil
}

type tonFinalizeLockScope struct {
	ExportID     int64
	UniqueGiftID int64
	OwnerUserID  int64
	SavedGiftID  int64
	Projection   userStarGiftProjectionLockScope
}

func loadTONFinalizeLockScope(ctx context.Context, db sqlcgen.DBTX, jobID int64) (tonFinalizeLockScope, error) {
	var scope tonFinalizeLockScope
	var msgID, upgradeMsgID int
	err := db.QueryRow(ctx, `SELECT e.id,e.unique_gift_id,e.owner_user_id,p.id,p.msg_id,p.upgrade_msg_id
FROM star_gift_ton_jobs j
JOIN star_gift_ton_exports e ON e.id=j.export_id
JOIN peer_star_gifts p ON p.unique_gift_id=e.unique_gift_id AND p.owner_peer_type='user' AND p.owner_peer_id=e.owner_user_id
WHERE j.id=$1`, jobID).Scan(&scope.ExportID, &scope.UniqueGiftID, &scope.OwnerUserID, &scope.SavedGiftID, &msgID, &upgradeMsgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return tonFinalizeLockScope{}, domain.ErrStarGiftTONExportStateConflict
	}
	if err != nil {
		return tonFinalizeLockScope{}, err
	}
	scope.Projection, err = loadUserStarGiftProjectionLockScope(ctx, db, scope.OwnerUserID, scope.SavedGiftID, msgID, upgradeMsgID)
	if err != nil {
		return tonFinalizeLockScope{}, err
	}
	return scope, nil
}

type tonProfileLockScope struct {
	ExportID     int64
	UniqueGiftID int64
	HostUserID   int64
}

func loadTONProfileLockScope(ctx context.Context, db sqlcgen.DBTX, jobID int64) (tonProfileLockScope, error) {
	var scope tonProfileLockScope
	err := db.QueryRow(ctx, `SELECT e.id,e.unique_gift_id,COALESCE(l.host_user_id,0)
FROM star_gift_ton_jobs j
JOIN star_gift_ton_exports e ON e.id=j.export_id
LEFT JOIN star_gift_ton_profile_links l ON l.export_id=e.id
WHERE j.id=$1`, jobID).Scan(&scope.ExportID, &scope.UniqueGiftID, &scope.HostUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return tonProfileLockScope{}, domain.ErrStarGiftTONExportStateConflict
	}
	if err != nil {
		return tonProfileLockScope{}, err
	}
	return scope, nil
}

type tonClaimLockScope struct {
	ChallengeID  int64
	ExportID     int64
	UniqueGiftID int64
	HostUserID   int64
}

func loadTONClaimLockScope(ctx context.Context, db sqlcgen.DBTX, challengeDigest []byte) (tonClaimLockScope, error) {
	var scope tonClaimLockScope
	err := db.QueryRow(ctx, `SELECT c.id,e.id,e.unique_gift_id,COALESCE(l.host_user_id,0)
FROM star_gift_ton_claims c
JOIN star_gift_ton_exports e ON e.id=c.export_id
LEFT JOIN star_gift_ton_profile_links l ON l.export_id=e.id
WHERE c.challenge_digest=$1`, challengeDigest).Scan(&scope.ChallengeID, &scope.ExportID, &scope.UniqueGiftID, &scope.HostUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return tonClaimLockScope{}, domain.ErrStarGiftTONExportStateConflict
	}
	if err != nil {
		return tonClaimLockScope{}, err
	}
	return scope, nil
}

func loadTONFinalizationResult(ctx context.Context, db sqlcgen.DBTX, exportID int64, out *domain.StarGiftTONFinalizationResult) error {
	export, found, err := tonExportByPredicate(ctx, db, "id=$1", exportID)
	if err != nil || !found {
		return domain.ErrStarGiftTONExportStateConflict
	}
	asset, found, err := tonAssetByExportID(ctx, db, exportID)
	if err != nil || !found {
		return domain.ErrStarGiftTONExportStateConflict
	}
	gift, found, err := NewStarGiftStore(db).UniqueByID(ctx, export.UniqueGiftID)
	if err != nil || !found {
		return domain.ErrStarGiftTONExportStateConflict
	}
	out.Export, out.Asset, out.Gift = export, asset, gift
	return nil
}

func lockStarGiftProfiles(ctx context.Context, tx pgx.Tx, profiles ...domain.Peer) error {
	keys := make([]string, 0, len(profiles))
	seen := make(map[string]struct{}, len(profiles))
	for _, profile := range profiles {
		if !validStarGiftOwner(profile) {
			return domain.ErrStarGiftOwnerInvalid
		}
		key := starGiftCollectionLockKey(profile)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
			return err
		}
	}
	return nil
}
