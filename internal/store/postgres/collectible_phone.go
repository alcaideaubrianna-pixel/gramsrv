package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"telesrv/internal/domain"
	"telesrv/internal/readmodelcache"
	"telesrv/internal/store"
	"telesrv/internal/store/postgres/sqlcgen"
)

const (
	collectiblePhoneOwnerCacheMaxEntries = 200000
	collectiblePhoneOwnerCacheTTL        = 30 * time.Minute
)

type collectiblePhoneOwnerCacheValue struct {
	asset domain.CollectiblePhone
	found bool
}

// CollectiblePhoneStore keeps only the rebuildable owner projection in memory.
// Mutations and read-model notifications invalidate it; PostgreSQL remains the
// durable ownership authority.
type CollectiblePhoneStore struct {
	db         sqlcgen.DBTX
	ownerCache *readmodelcache.Cache[int64, collectiblePhoneOwnerCacheValue]
	// tonStartingGrant only matters to PurchaseCollectiblePhoneWithDelivery: a
	// buyer paying in TON for the first time gets the same lazy starting grant
	// the Star Gift TON ledger already gives everyone.
	tonStartingGrant int64
}

// CollectiblePhoneOption configures optional CollectiblePhoneStore behaviour.
type CollectiblePhoneOption func(*CollectiblePhoneStore)

// WithCollectiblePhoneTONStartingGrant sets the lazy TON starting-grant amount
// applied the first time a buyer pays in TON. It must match the node's
// configured domain.Config.StarGiftTONStartingGrant (nanoton).
func WithCollectiblePhoneTONStartingGrant(nanoton int64) CollectiblePhoneOption {
	return func(s *CollectiblePhoneStore) { s.tonStartingGrant = nanoton }
}

func NewCollectiblePhoneStore(db sqlcgen.DBTX, opts ...CollectiblePhoneOption) *CollectiblePhoneStore {
	s := &CollectiblePhoneStore{
		db: db,
		ownerCache: readmodelcache.New(readmodelcache.Config[int64, collectiblePhoneOwnerCacheValue]{
			MaxEntries: collectiblePhoneOwnerCacheMaxEntries,
			TTL:        collectiblePhoneOwnerCacheTTL,
		}),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

var _ store.CollectiblePhoneStore = (*CollectiblePhoneStore)(nil)

const collectiblePhoneColumns = `id, phone, tier, status, owner_user_id, purchase_date,
currency, amount, crypto_currency, crypto_amount, url, original_owner_user_id,
transfer_count, version, created_at, updated_at`

type collectiblePhoneScanner interface{ Scan(...any) error }

func scanCollectiblePhone(row collectiblePhoneScanner) (domain.CollectiblePhone, error) {
	var a domain.CollectiblePhone
	err := row.Scan(&a.ID, &a.Phone, &a.Tier, &a.Status, &a.OwnerUserID, &a.PurchaseDate,
		&a.Currency, &a.Amount, &a.CryptoCurrency, &a.CryptoAmount, &a.URL,
		&a.OriginalOwnerUserID, &a.TransferCount, &a.Version, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

func normalizePhoneCommand(phone, actor, reason, key string) (string, string, string, string) {
	return domain.NormalizeCollectiblePhone(phone), strings.TrimSpace(actor), strings.TrimSpace(reason), strings.TrimSpace(key)
}

func ensureCollectiblePhoneOwner(ctx context.Context, tx pgx.Tx, userID int64) error {
	var bot, deleted bool
	err := tx.QueryRow(ctx, `SELECT is_bot, deleted_at IS NOT NULL FROM users WHERE id=$1`, userID).Scan(&bot, &deleted)
	if errors.Is(err, pgx.ErrNoRows) || bot || deleted {
		return domain.ErrCollectiblePhoneInvalid
	}
	if err != nil {
		return fmt.Errorf("check collectible phone owner: %w", err)
	}
	var occupied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM collectible_phones WHERE status='owned' AND owner_user_id=$1)`, userID).Scan(&occupied); err != nil {
		return fmt.Errorf("check collectible phone owner limit: %w", err)
	}
	if occupied {
		return domain.ErrCollectiblePhoneOwnerLimit
	}
	return nil
}

func replayCollectiblePhone(ctx context.Context, tx pgx.Tx, key string) (domain.CollectiblePhone, bool, error) {
	if key == "" {
		return domain.CollectiblePhone{}, false, nil
	}
	row := tx.QueryRow(ctx, `SELECT cp.id, cp.phone, cp.tier, cp.status, cp.owner_user_id, cp.purchase_date,
cp.currency, cp.amount, cp.crypto_currency, cp.crypto_amount, cp.url, cp.original_owner_user_id,
cp.transfer_count, cp.version, cp.created_at, cp.updated_at
FROM collectible_phone_transfers t JOIN collectible_phones cp ON cp.id=t.collectible_id
WHERE t.command_key=$1`, key)
	a, err := scanCollectiblePhone(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.CollectiblePhone{}, false, nil
	}
	if err != nil {
		return domain.CollectiblePhone{}, false, fmt.Errorf("replay collectible phone command: %w", err)
	}
	return a, true, nil
}

func insertCollectiblePhoneTransfer(ctx context.Context, tx pgx.Tx, id int64, kind domain.CollectibleUsernameTransferKind, from, to int64, currency string, amount int64, actor, reason, key string) error {
	var nullableKey any
	if key != "" {
		nullableKey = key
	}
	_, err := tx.Exec(ctx, `INSERT INTO collectible_phone_transfers
(collectible_id,kind,from_user_id,to_user_id,currency,amount,actor,reason,command_key,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,now())`, id, string(kind), from, to, currency, amount, actor, reason, nullableKey)
	if err != nil {
		return fmt.Errorf("insert collectible phone transfer: %w", err)
	}
	return nil
}

func (s *CollectiblePhoneStore) MintCollectiblePhoneWithDelivery(ctx context.Context, req domain.MintCollectiblePhoneRequest, effects store.DeliveryEffectsBuilder[store.CollectiblePhoneDeliverySnapshot]) (domain.CollectiblePhone, bool, error) {
	if s == nil || s.db == nil {
		return domain.CollectiblePhone{}, false, fmt.Errorf("collectible phone store is not configured")
	}
	if effects == nil {
		return domain.CollectiblePhone{}, false, store.ErrDeliveryOutboxRequired
	}
	req.Phone, req.Actor, req.Reason, req.CommandKey = normalizePhoneCommand(req.Phone, req.Actor, req.Reason, req.CommandKey)
	if req.Tier == "" {
		req.Tier = domain.CollectiblePhoneTierStandard
	}
	if req.Currency == "" {
		req.Currency = domain.CollectibleCurrencyUSD
	}
	if req.URL == "" {
		req.URL = "https://fragment.com/number/" + req.Phone
	}
	if err := req.Validate(); err != nil {
		return domain.CollectiblePhone{}, false, err
	}
	var out domain.CollectiblePhone
	created := false
	err := withTx(ctx, s.db, "mint collectible phone", func(tx pgx.Tx) error {
		if replay, found, err := replayCollectiblePhone(ctx, tx, req.CommandKey); err != nil {
			return err
		} else if found {
			out = replay
			return nil
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM collectible_phones WHERE phone=$1 AND status<>'burned')`, req.Phone).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return domain.ErrCollectiblePhoneInvalid
		}
		if req.OwnerUserID > 0 {
			if err := ensureCollectiblePhoneOwner(ctx, tx, req.OwnerUserID); err != nil {
				return err
			}
		}
		status := domain.CollectibleUsernameStatusVault
		if req.OwnerUserID > 0 {
			status = domain.CollectibleUsernameStatusOwned
		}
		purchaseDate := req.PurchaseDate.UTC()
		if req.PurchaseDate.IsZero() {
			purchaseDate = time.Now().UTC()
		}
		row := tx.QueryRow(ctx, `INSERT INTO collectible_phones
(phone,tier,status,owner_user_id,purchase_date,currency,amount,crypto_currency,crypto_amount,url,
 original_owner_user_id,transfer_count,version,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$4,0,1,now(),now()) RETURNING `+collectiblePhoneColumns,
			req.Phone, string(req.Tier), string(status), req.OwnerUserID, purchaseDate, req.Currency, req.Amount, req.CryptoCurrency, req.CryptoAmount, req.URL)
		var err error
		out, err = scanCollectiblePhone(row)
		if err != nil {
			if isUniqueViolation(err) {
				return domain.ErrCollectiblePhoneOwnerLimit
			}
			return err
		}
		if err := insertCollectiblePhoneTransfer(ctx, tx, out.ID, domain.CollectibleUsernameKindMint, 0, req.OwnerUserID, req.Currency, req.Amount, req.Actor, req.Reason, req.CommandKey); err != nil {
			return err
		}
		if err := applyCollectiblePhoneDeliveryTx(ctx, tx, out, []int64{out.OwnerUserID}, effects); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err == nil && created {
		s.InvalidateCollectiblePhoneReadModel(out.OwnerUserID)
	}
	return out, created, err
}

func (s *CollectiblePhoneStore) UpdateCollectiblePhonePriceWithDelivery(ctx context.Context, req domain.UpdateCollectiblePhonePriceRequest, effects store.DeliveryEffectsBuilder[store.CollectiblePhoneDeliverySnapshot]) (domain.CollectiblePhone, bool, error) {
	if s == nil || s.db == nil {
		return domain.CollectiblePhone{}, false, fmt.Errorf("collectible phone store is not configured")
	}
	if effects == nil {
		return domain.CollectiblePhone{}, false, store.ErrDeliveryOutboxRequired
	}
	req.Phone = domain.NormalizeCollectiblePhone(req.Phone)
	req.Currency = strings.ToUpper(strings.TrimSpace(req.Currency))
	req.CryptoCurrency = strings.ToUpper(strings.TrimSpace(req.CryptoCurrency))
	req.Actor = strings.TrimSpace(req.Actor)
	req.Reason = strings.TrimSpace(req.Reason)
	if err := req.Validate(); err != nil {
		return domain.CollectiblePhone{}, false, err
	}
	var out domain.CollectiblePhone
	changed := false
	err := withTx(ctx, s.db, "update collectible phone price", func(tx pgx.Tx) error {
		a, err := scanCollectiblePhone(tx.QueryRow(ctx, `SELECT `+collectiblePhoneColumns+` FROM collectible_phones WHERE phone=$1 AND status<>'burned' FOR UPDATE`, req.Phone))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrCollectiblePhoneNotFound
		}
		if err != nil {
			return err
		}
		if a.Currency == req.Currency && a.Amount == req.Amount && a.CryptoCurrency == req.CryptoCurrency && a.CryptoAmount == req.CryptoAmount {
			out = a
			return nil
		}
		out, err = scanCollectiblePhone(tx.QueryRow(ctx, `UPDATE collectible_phones SET currency=$2, amount=$3,
crypto_currency=$4, crypto_amount=$5, version=version+1, updated_at=now()
WHERE id=$1 RETURNING `+collectiblePhoneColumns, a.ID, req.Currency, req.Amount, req.CryptoCurrency, req.CryptoAmount))
		if err != nil {
			return err
		}
		if err := applyCollectiblePhoneDeliveryTx(ctx, tx, out, []int64{out.OwnerUserID}, effects); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err == nil && changed {
		s.InvalidateCollectiblePhoneReadModel(out.OwnerUserID)
	}
	return out, changed, err
}

func (s *CollectiblePhoneStore) TransferCollectiblePhoneWithDelivery(ctx context.Context, req domain.TransferCollectiblePhoneRequest, effects store.DeliveryEffectsBuilder[store.CollectiblePhoneDeliverySnapshot]) (domain.CollectiblePhone, bool, error) {
	if effects == nil {
		return domain.CollectiblePhone{}, false, store.ErrDeliveryOutboxRequired
	}
	req.Phone, req.Actor, req.Reason, req.CommandKey = normalizePhoneCommand(req.Phone, req.Actor, req.Reason, req.CommandKey)
	if err := req.Validate(); err != nil {
		return domain.CollectiblePhone{}, false, err
	}
	var out domain.CollectiblePhone
	var previousOwnerUserID int64
	changed := false
	err := withTx(ctx, s.db, "transfer collectible phone", func(tx pgx.Tx) error {
		if replay, found, err := replayCollectiblePhone(ctx, tx, req.CommandKey); err != nil {
			return err
		} else if found {
			out = replay
			return nil
		}
		a, err := scanCollectiblePhone(tx.QueryRow(ctx, `SELECT `+collectiblePhoneColumns+` FROM collectible_phones WHERE phone=$1 AND status<>'burned' FOR UPDATE`, req.Phone))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrCollectiblePhoneNotFound
		}
		if err != nil {
			return err
		}
		if a.OwnerUserID == req.ToUserID {
			out = a
			return nil
		}
		previousOwnerUserID = a.OwnerUserID
		if err := ensureCollectiblePhoneOwner(ctx, tx, req.ToUserID); err != nil {
			return err
		}
		original := a.OriginalOwnerUserID
		if original == 0 {
			original = req.ToUserID
		}
		out, err = scanCollectiblePhone(tx.QueryRow(ctx, `UPDATE collectible_phones SET status='owned', owner_user_id=$2,
original_owner_user_id=$3, transfer_count=transfer_count+1, version=version+1, updated_at=now()
WHERE id=$1 RETURNING `+collectiblePhoneColumns, a.ID, req.ToUserID, original))
		if err != nil {
			if isUniqueViolation(err) {
				return domain.ErrCollectiblePhoneOwnerLimit
			}
			return err
		}
		if err := insertCollectiblePhoneTransfer(ctx, tx, a.ID, domain.CollectibleUsernameKindTransfer, a.OwnerUserID, req.ToUserID, "", 0, req.Actor, req.Reason, req.CommandKey); err != nil {
			return err
		}
		if err := applyCollectiblePhoneDeliveryTx(ctx, tx, out, []int64{a.OwnerUserID, out.OwnerUserID}, effects); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err == nil && changed {
		s.InvalidateCollectiblePhoneReadModel(previousOwnerUserID)
		s.InvalidateCollectiblePhoneReadModel(out.OwnerUserID)
	}
	return out, changed, err
}

// PurchaseCollectiblePhoneWithDelivery buys a vault-held number at its
// recorded CryptoAmount TON price: it debits the buyer through the same
// shared ledger primitive Star Gift purchases use, then moves ownership
// exactly like TransferCollectiblePhoneWithDelivery does. This is the
// self-service, no-admin-involved counterpart to TransferCollectiblePhone.
func (s *CollectiblePhoneStore) PurchaseCollectiblePhoneWithDelivery(ctx context.Context, req domain.PurchaseCollectiblePhoneRequest, effects store.DeliveryEffectsBuilder[store.CollectiblePhoneDeliverySnapshot]) (domain.CollectiblePhone, bool, error) {
	if effects == nil {
		return domain.CollectiblePhone{}, false, store.ErrDeliveryOutboxRequired
	}
	req.Phone = domain.NormalizeCollectiblePhone(req.Phone)
	req.Actor = strings.TrimSpace(req.Actor)
	req.Reason = strings.TrimSpace(req.Reason)
	req.CommandKey = strings.TrimSpace(req.CommandKey)
	if err := req.Validate(); err != nil {
		return domain.CollectiblePhone{}, false, err
	}
	var out domain.CollectiblePhone
	changed := false
	err := withTx(ctx, s.db, "purchase collectible phone", func(tx pgx.Tx) error {
		if replay, found, err := replayCollectiblePhone(ctx, tx, req.CommandKey); err != nil {
			return err
		} else if found {
			out = replay
			return nil
		}
		a, err := scanCollectiblePhone(tx.QueryRow(ctx, `SELECT `+collectiblePhoneColumns+` FROM collectible_phones WHERE phone=$1 AND status='vault' FOR UPDATE`, req.Phone))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrCollectiblePhoneNotForSale
		}
		if err != nil {
			return err
		}
		if a.CryptoCurrency != domain.CollectibleCryptoCurrencyTON || a.CryptoAmount <= 0 {
			return domain.ErrCollectiblePhoneNotForSale
		}
		if err := ensureCollectiblePhoneOwner(ctx, tx, req.BuyerUserID); err != nil {
			return err
		}
		now := time.Now().UTC()
		price := domain.StarGiftAmount{Currency: domain.StarGiftCurrencyTON, Amount: a.CryptoAmount}
		if _, err := debitLedgerAmountTx(ctx, tx, req.BuyerUserID, price, domain.StarsReasonCollectible,
			domain.Peer{}, int(now.Unix()), "Collectible phone number", s.tonStartingGrant); err != nil {
			return err
		}
		original := a.OriginalOwnerUserID
		if original == 0 {
			original = req.BuyerUserID
		}
		out, err = scanCollectiblePhone(tx.QueryRow(ctx, `UPDATE collectible_phones SET status='owned', owner_user_id=$2,
original_owner_user_id=$3, purchase_date=$4, transfer_count=transfer_count+1, version=version+1, updated_at=$4
WHERE id=$1 RETURNING `+collectiblePhoneColumns, a.ID, req.BuyerUserID, original, now))
		if err != nil {
			if isUniqueViolation(err) {
				return domain.ErrCollectiblePhoneOwnerLimit
			}
			return err
		}
		if err := insertCollectiblePhoneTransfer(ctx, tx, a.ID, domain.CollectibleUsernameKindTransfer, a.OwnerUserID, req.BuyerUserID,
			string(domain.CollectibleCryptoCurrencyTON), a.CryptoAmount, req.Actor, req.Reason, req.CommandKey); err != nil {
			return err
		}
		if err := applyCollectiblePhoneDeliveryTx(ctx, tx, out, []int64{req.BuyerUserID}, effects); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err == nil && changed {
		s.InvalidateCollectiblePhoneReadModel(out.OwnerUserID)
	}
	return out, changed, err
}

func (s *CollectiblePhoneStore) RevokeCollectiblePhoneWithDelivery(ctx context.Context, req domain.RevokeCollectiblePhoneRequest, effects store.DeliveryEffectsBuilder[store.CollectiblePhoneDeliverySnapshot]) (domain.CollectiblePhone, bool, error) {
	if effects == nil {
		return domain.CollectiblePhone{}, false, store.ErrDeliveryOutboxRequired
	}
	req.Phone, req.Actor, req.Reason, req.CommandKey = normalizePhoneCommand(req.Phone, req.Actor, req.Reason, req.CommandKey)
	if err := req.Validate(); err != nil {
		return domain.CollectiblePhone{}, false, err
	}
	var out domain.CollectiblePhone
	var previousOwnerUserID int64
	changed := false
	err := withTx(ctx, s.db, "revoke collectible phone", func(tx pgx.Tx) error {
		if replay, found, err := replayCollectiblePhone(ctx, tx, req.CommandKey); err != nil {
			return err
		} else if found {
			out = replay
			return nil
		}
		a, err := scanCollectiblePhone(tx.QueryRow(ctx, `SELECT `+collectiblePhoneColumns+` FROM collectible_phones WHERE phone=$1 AND status<>'burned' FOR UPDATE`, req.Phone))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrCollectiblePhoneNotFound
		}
		if err != nil {
			return err
		}
		if a.Status == domain.CollectibleUsernameStatusVault && !req.Burn {
			out = a
			return nil
		}
		previousOwnerUserID = a.OwnerUserID
		status := domain.CollectibleUsernameStatusVault
		kind := domain.CollectibleUsernameKindRevoke
		if req.Burn {
			status = domain.CollectibleUsernameStatusBurned
			kind = domain.CollectibleUsernameKindBurn
		}
		out, err = scanCollectiblePhone(tx.QueryRow(ctx, `UPDATE collectible_phones SET status=$2, owner_user_id=0,
version=version+1, updated_at=now() WHERE id=$1 RETURNING `+collectiblePhoneColumns, a.ID, string(status)))
		if err != nil {
			return err
		}
		if err := insertCollectiblePhoneTransfer(ctx, tx, a.ID, kind, a.OwnerUserID, 0, "", 0, req.Actor, req.Reason, req.CommandKey); err != nil {
			return err
		}
		if err := applyCollectiblePhoneDeliveryTx(ctx, tx, out, []int64{a.OwnerUserID}, effects); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err == nil && changed {
		s.InvalidateCollectiblePhoneReadModel(previousOwnerUserID)
	}
	return out, changed, err
}

func (s *CollectiblePhoneStore) DeleteCollectiblePhoneWithDelivery(ctx context.Context, req domain.DeleteCollectiblePhoneRequest, effects store.DeliveryEffectsBuilder[store.CollectiblePhoneDeliverySnapshot]) (bool, error) {
	if effects == nil {
		return false, store.ErrDeliveryOutboxRequired
	}
	req.Phone, req.Actor, req.Reason, req.CommandKey = normalizePhoneCommand(req.Phone, req.Actor, req.Reason, req.CommandKey)
	if err := req.Validate(); err != nil {
		return false, err
	}
	deleted := false
	var previousOwnerUserID int64
	err := withTx(ctx, s.db, "delete collectible phone with delivery", func(tx pgx.Tx) error {
		asset, err := scanCollectiblePhone(tx.QueryRow(ctx, `SELECT `+collectiblePhoneColumns+` FROM collectible_phones WHERE phone=$1 ORDER BY id DESC LIMIT 1 FOR UPDATE`, req.Phone))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM collectible_phones WHERE id=$1`, asset.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("delete collectible phone lost row lock")
		}
		if err := applyCollectiblePhoneDeliveryTx(ctx, tx, asset, []int64{asset.OwnerUserID}, effects); err != nil {
			return err
		}
		previousOwnerUserID = asset.OwnerUserID
		deleted = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("delete collectible phone: %w", err)
	}
	if deleted {
		s.InvalidateCollectiblePhoneReadModel(previousOwnerUserID)
	}
	return deleted, nil
}

func applyCollectiblePhoneDeliveryTx(
	ctx context.Context,
	tx pgx.Tx,
	asset domain.CollectiblePhone,
	userIDs []int64,
	effects store.DeliveryEffectsBuilder[store.CollectiblePhoneDeliverySnapshot],
) error {
	users, err := userAudienceSnapshots(ctx, tx, userIDs, maxModerationFlagAudience)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		return nil
	}
	snapshot := store.CollectiblePhoneDeliverySnapshot{Asset: asset, Users: users}
	intents, err := effects(snapshot)
	if err != nil {
		return fmt.Errorf("build collectible phone delivery: %w", err)
	}
	if err := store.ValidateCollectiblePhoneDeliveryEffects(snapshot, intents); err != nil {
		return err
	}
	return applyAbsoluteDeliveryEffectsTx(ctx, tx, intents)
}

func (s *CollectiblePhoneStore) CollectiblePhone(ctx context.Context, phone string) (domain.CollectiblePhone, error) {
	phone = domain.NormalizeCollectiblePhone(phone)
	if !domain.ValidCollectiblePhone(phone) {
		return domain.CollectiblePhone{}, domain.ErrCollectiblePhoneInvalid
	}
	a, err := scanCollectiblePhone(s.db.QueryRow(ctx, `SELECT `+collectiblePhoneColumns+` FROM collectible_phones WHERE phone=$1 AND status<>'burned' ORDER BY id DESC LIMIT 1`, phone))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.CollectiblePhone{}, domain.ErrCollectiblePhoneNotFound
	}
	return a, err
}

func (s *CollectiblePhoneStore) CollectiblePhoneByID(ctx context.Context, id int64) (domain.CollectiblePhone, error) {
	a, err := scanCollectiblePhone(s.db.QueryRow(ctx, `SELECT `+collectiblePhoneColumns+` FROM collectible_phones WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.CollectiblePhone{}, domain.ErrCollectiblePhoneNotFound
	}
	return a, err
}

func (s *CollectiblePhoneStore) OwnedCollectiblePhones(ctx context.Context, userIDs []int64) (map[int64]domain.CollectiblePhone, error) {
	out := make(map[int64]domain.CollectiblePhone)
	if s == nil || s.db == nil || len(userIDs) == 0 {
		return out, nil
	}
	ids := uniqueCollectiblePhoneOwnerIDs(userIDs)
	values, err := s.ownerCache.GetOrLoadBatch(
		ctx,
		ids,
		func(int64) (int64, bool) { return 0, true },
		func(loadCtx context.Context, missing []int64) (map[int64]collectiblePhoneOwnerCacheValue, error) {
			loaded := make(map[int64]collectiblePhoneOwnerCacheValue, len(missing))
			for _, userID := range missing {
				loaded[userID] = collectiblePhoneOwnerCacheValue{}
			}
			rows, queryErr := s.db.Query(loadCtx, `SELECT `+collectiblePhoneColumns+` FROM collectible_phones WHERE status='owned' AND owner_user_id=ANY($1::bigint[])`, missing)
			if queryErr != nil {
				return nil, queryErr
			}
			defer rows.Close()
			for rows.Next() {
				asset, scanErr := scanCollectiblePhone(rows)
				if scanErr != nil {
					return nil, scanErr
				}
				loaded[asset.OwnerUserID] = collectiblePhoneOwnerCacheValue{asset: asset, found: true}
			}
			if rowsErr := rows.Err(); rowsErr != nil {
				return nil, rowsErr
			}
			return loaded, nil
		},
	)
	if err != nil {
		return nil, err
	}
	for userID, value := range values {
		if value.found {
			out[userID] = value.asset
		}
	}
	return out, nil
}

// InvalidateCollectiblePhoneReadModel applies one committed owner-key change.
func (s *CollectiblePhoneStore) InvalidateCollectiblePhoneReadModel(userID int64) {
	if s != nil && s.ownerCache != nil && userID > 0 {
		s.ownerCache.Invalidate(userID)
	}
}

// FlushCollectiblePhoneReadModel closes notification disconnect windows and is
// used after listener reconnects to close a lost-notification window.
func (s *CollectiblePhoneStore) FlushCollectiblePhoneReadModel() {
	if s != nil && s.ownerCache != nil {
		s.ownerCache.Flush()
	}
}

func uniqueCollectiblePhoneOwnerIDs(values []int64) []int64 {
	out := make([]int64, 0, len(values))
	seen := make(map[int64]struct{}, len(values))
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func (s *CollectiblePhoneStore) ListCollectiblePhones(ctx context.Context, f domain.CollectiblePhoneFilter) ([]domain.CollectiblePhone, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 200 {
		f.Limit = 200
	}
	query := strings.TrimPrefix(domain.NormalizeCollectiblePhone(f.Query), "")
	rows, err := s.db.Query(ctx, `SELECT `+collectiblePhoneColumns+` FROM collectible_phones
WHERE ($1='' OR status=$1) AND ($2='' OR tier=$2) AND ($3::bigint=0 OR owner_user_id=$3)
AND ($4='' OR phone LIKE $4||'%') AND ($5::bigint=0 OR id<$5) ORDER BY id DESC LIMIT $6`,
		string(f.Status), string(f.Tier), f.OwnerUserID, query, f.BeforeID, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.CollectiblePhone, 0, f.Limit)
	for rows.Next() {
		a, err := scanCollectiblePhone(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *CollectiblePhoneStore) CollectiblePhoneTransfers(ctx context.Context, collectibleID int64, limit int) ([]domain.CollectiblePhoneTransfer, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.db.Query(ctx, `SELECT id,collectible_id,kind,from_user_id,to_user_id,currency,amount,actor,reason,COALESCE(command_key,''),created_at
FROM collectible_phone_transfers WHERE collectible_id=$1 ORDER BY id DESC LIMIT $2`, collectibleID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.CollectiblePhoneTransfer, 0)
	for rows.Next() {
		var v domain.CollectiblePhoneTransfer
		if err := rows.Scan(&v.ID, &v.CollectibleID, &v.Kind, &v.FromUserID, &v.ToUserID, &v.Currency, &v.Amount, &v.Actor, &v.Reason, &v.CommandKey, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
