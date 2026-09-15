package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"telesrv/internal/domain"
	"telesrv/internal/store/postgres/sqlcgen"
)

var errUserStarGiftProjectionLockScopeChanged = errors.New("user star gift projection lock scope changed")

// userStarGiftProjectionLockScope is the complete user lock set for retiring
// every private-message alias of one user-owned gift. It must be loaded before
// a mutation transaction starts, acquired before any row lock, and validated
// again immediately after the advisory locks are held. A changed scope fails
// closed; callers must not continue with a partial set or retry inside the same
// transaction.
type userStarGiftProjectionLockScope struct {
	OwnerUserID  int64
	SavedGiftID  int64
	MsgID        int
	UpgradeMsgID int
	UserIDs      []int64
}

func loadUserStarGiftProjectionLockScope(
	ctx context.Context,
	db sqlcgen.DBTX,
	ownerUserID, savedGiftID int64,
	msgID, upgradeMsgID int,
) (userStarGiftProjectionLockScope, error) {
	scope := userStarGiftProjectionLockScope{
		OwnerUserID:  ownerUserID,
		SavedGiftID:  savedGiftID,
		MsgID:        msgID,
		UpgradeMsgID: upgradeMsgID,
	}
	if db == nil || ownerUserID <= 0 || savedGiftID <= 0 || msgID < 0 || upgradeMsgID < 0 {
		return userStarGiftProjectionLockScope{}, errUserStarGiftProjectionLockScopeChanged
	}
	var err error
	scope.UserIDs, err = userStarGiftProjectionUserIDs(ctx, db, ownerUserID, savedGiftID, msgID, upgradeMsgID)
	if err != nil {
		return userStarGiftProjectionLockScope{}, fmt.Errorf("load user star gift projection lock scope: %w", err)
	}
	return scope, nil
}

func loadSavedUserStarGiftProjectionLockScope(
	ctx context.Context,
	db sqlcgen.DBTX,
	saved domain.SavedStarGift,
) (userStarGiftProjectionLockScope, error) {
	if saved.Owner.Type != domain.PeerTypeUser {
		return userStarGiftProjectionLockScope{}, errUserStarGiftProjectionLockScopeChanged
	}
	return loadUserStarGiftProjectionLockScope(ctx, db, saved.Owner.ID, saved.ID, saved.MsgID, saved.UpgradeMsgID)
}

func userStarGiftProjectionUserIDs(
	ctx context.Context,
	db sqlcgen.DBTX,
	ownerUserID, savedGiftID int64,
	msgID, upgradeMsgID int,
) ([]int64, error) {
	userIDs := []int64{ownerUserID}
	rows, err := db.Query(ctx, `SELECT DISTINCT m.peer_id
FROM message_boxes m
WHERE m.owner_user_id=$1 AND m.peer_type='user' AND m.peer_id>0 AND NOT m.deleted
  AND (m.box_id=$3 OR m.box_id=$4 OR EXISTS (
      SELECT 1 FROM star_gift_user_message_refs r
      WHERE r.owner_user_id=$1 AND r.saved_gift_id=$2 AND r.msg_id=m.box_id
  ))
ORDER BY m.peer_id`, ownerUserID, savedGiftID, msgID, upgradeMsgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var userID int64
		if err := rows.Scan(&userID); err != nil {
			return nil, err
		}
		userIDs = append(userIDs, userID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(userIDs, func(i, j int) bool { return userIDs[i] < userIDs[j] })
	deduped := userIDs[:0]
	for _, userID := range userIDs {
		if len(deduped) == 0 || deduped[len(deduped)-1] != userID {
			deduped = append(deduped, userID)
		}
	}
	return deduped, nil
}

func validateUserStarGiftProjectionLockScope(
	ctx context.Context,
	db sqlcgen.DBTX,
	scope userStarGiftProjectionLockScope,
) error {
	if scope.OwnerUserID <= 0 || scope.SavedGiftID <= 0 || len(scope.UserIDs) == 0 {
		return errUserStarGiftProjectionLockScopeChanged
	}
	current, err := userStarGiftProjectionUserIDs(ctx, db, scope.OwnerUserID, scope.SavedGiftID, scope.MsgID, scope.UpgradeMsgID)
	if err != nil {
		return fmt.Errorf("validate user star gift projection lock scope: %w", err)
	}
	if !equalUserIDSets(current, scope.UserIDs) {
		return errUserStarGiftProjectionLockScopeChanged
	}
	return nil
}

// lockUserStarGiftProjectionUsers is the first locking operation for direct
// lifecycle transactions. CompleteStarGiftWithdrawal deliberately separates
// this advisory fence from validation so a completion racing an already
// committed idempotent replay can inspect status before deleted aliases make
// the old preflight scope differ.
func lockUserStarGiftProjectionUsers(
	ctx context.Context,
	tx pgx.Tx,
	scope userStarGiftProjectionLockScope,
) error {
	if scope.OwnerUserID <= 0 || scope.SavedGiftID <= 0 || len(scope.UserIDs) == 0 {
		return errUserStarGiftProjectionLockScopeChanged
	}
	return lockUsersForUpdate(ctx, tx, scope.UserIDs...)
}

// lockUserStarGiftProjectionScope acquires and immediately validates the full
// scope for direct mutations without a terminal replay row. Private-send
// mutations pass UserIDs to the send hook and validate as the hook's first
// operation.
func lockUserStarGiftProjectionScope(
	ctx context.Context,
	tx pgx.Tx,
	scope userStarGiftProjectionLockScope,
) error {
	if err := lockUserStarGiftProjectionUsers(ctx, tx, scope); err != nil {
		return err
	}
	return validateUserStarGiftProjectionLockScope(ctx, tx, scope)
}

func (scope userStarGiftProjectionLockScope) matches(saved domain.SavedStarGift) bool {
	return saved.Owner == (domain.Peer{Type: domain.PeerTypeUser, ID: scope.OwnerUserID}) &&
		saved.ID == scope.SavedGiftID && saved.MsgID == scope.MsgID && saved.UpgradeMsgID == scope.UpgradeMsgID
}

func equalUserIDSets(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// retireUserStarGiftMessagesTx closes every user-scoped unique-gift action
// emitted for the source ownership epoch. Ownership moves and terminal export
// must not leave an older chat card with Craft/transfer/resale capabilities.
// The aggregate mutation and all message edits share one transaction and each
// visible box receives its own durable pts/event/outbox entry.
//
// soldFor is nil for every ownership change except a completed marketplace
// sale (PurchaseResaleStarGift), where it carries the price actually paid.
// Without it this retirement unconditionally blanks ResaleAmount on the old
// card, so the former owner's retired copy read as a bare "transferred" with
// no price even when they were just paid for it.
func (s *StarGiftLifecycleStore) retireUserStarGiftMessagesTx(
	ctx context.Context,
	tx pgx.Tx,
	soldFor *domain.StarGiftAmount,
	source domain.SavedStarGift,
	current domain.UniqueStarGift,
	lockScope userStarGiftProjectionLockScope,
	date int,
) ([]domain.EditedMessageForUser, error) {
	if s == nil || s.messages == nil || source.Owner.Type != domain.PeerTypeUser || source.Owner.ID <= 0 ||
		source.ID <= 0 || source.UniqueGiftID <= 0 || current.ID != source.UniqueGiftID || date <= 0 ||
		!lockScope.matches(source) {
		return nil, domain.ErrStarGiftTransferUnavailable
	}

	messageIDs := map[int]struct{}{}
	if source.MsgID > 0 {
		messageIDs[source.MsgID] = struct{}{}
	}
	if source.UpgradeMsgID > 0 {
		messageIDs[source.UpgradeMsgID] = struct{}{}
	}
	rows, err := tx.Query(ctx, `
SELECT msg_id FROM star_gift_user_message_refs
WHERE owner_user_id=$1 AND saved_gift_id=$2
ORDER BY msg_id`, source.Owner.ID, source.ID)
	if err != nil {
		return nil, fmt.Errorf("list star gift message projections: %w", err)
	}
	for rows.Next() {
		var msgID int
		if err := rows.Scan(&msgID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan star gift message projection: %w", err)
		}
		if msgID > 0 {
			messageIDs[msgID] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate star gift message projections: %w", err)
	}
	rows.Close()

	ids := make([]int, 0, len(messageIDs))
	for msgID := range messageIDs {
		ids = append(ids, msgID)
	}
	sort.Ints(ids)

	q := sqlcgen.New(tx)
	edits := make([]domain.EditedMessageForUser, 0, len(ids)*2)
	seenPrivateMessages := make(map[string]struct{}, len(ids))
	for _, msgID := range ids {
		var peerType string
		var peerID int64
		err := tx.QueryRow(ctx, `
SELECT peer_type,peer_id FROM message_boxes
WHERE owner_user_id=$1 AND box_id=$2 AND NOT deleted
FOR UPDATE`, source.Owner.ID, msgID).Scan(&peerType, &peerID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("lock star gift message projection: %w", err)
		}
		if peerType != string(domain.PeerTypeUser) || peerID <= 0 {
			return nil, fmt.Errorf("star gift message projection %d is not private", msgID)
		}
		target, err := q.GetMessageBoxForEdit(ctx, sqlcgen.GetMessageBoxForEditParams{
			OwnerUserID: source.Owner.ID, BoxID: int32(msgID), PeerType: peerType, PeerID: peerID,
		})
		if err != nil {
			return nil, fmt.Errorf("load star gift message projection: %w", err)
		}
		logicalKey := fmt.Sprintf("%d:%d", target.MessageSenderID, target.PrivateMessageID)
		if _, duplicate := seenPrivateMessages[logicalKey]; duplicate {
			continue
		}
		seenPrivateMessages[logicalKey] = struct{}{}

		boxes, err := q.ListVisibleMessageBoxesByPrivateMessage(ctx, sqlcgen.ListVisibleMessageBoxesByPrivateMessageParams{
			OwnerUserIds:    privateMessageOwnerIDs(source.Owner.ID, peerID),
			MessageSenderID: target.MessageSenderID, PrivateMessageID: target.PrivateMessageID,
		})
		if err != nil {
			return nil, fmt.Errorf("list visible star gift message projections: %w", err)
		}
		var privateMediaJSON []byte
		matched := false
		for _, box := range boxes {
			media, err := decodeMessageMedia(box.MediaJson)
			if err != nil {
				return nil, fmt.Errorf("decode star gift message projection: %w", err)
			}
			if media == nil || media.Kind != domain.MessageMediaKindService || media.ServiceAction == nil ||
				media.ServiceAction.Kind != domain.MessageServiceActionStarGiftUnique || media.ServiceAction.StarGiftUnique == nil ||
				media.ServiceAction.StarGiftUnique.Gift.ID != current.ID {
				continue
			}
			matched = true
			action := media.ServiceAction.StarGiftUnique
			retiredGift := current
			retiredGift.CraftChancePermille = 0
			retiredGift.ResellAmount = nil
			action.Gift = retiredGift
			action.Peer = domain.Peer{}
			action.SavedID = 0
			action.Saved = false
			if validLifecyclePeer(current.Owner) && current.Owner != source.Owner {
				action.Transferred = true
			}
			action.CanExportAt = 0
			action.TransferStars = 0
			action.ResaleAmount = soldFor
			action.CanTransferAt = 0
			action.CanResellAt = 0
			action.DropOriginalDetailsStars = 0
			action.CanCraftAt = 0

			mediaJSON, err := encodeMessageMedia(media)
			if err != nil {
				return nil, fmt.Errorf("encode retired star gift projection: %w", err)
			}
			pts, err := s.messages.reservePts(ctx, tx, box.OwnerUserID)
			if err != nil {
				return nil, fmt.Errorf("allocate retired star gift pts: %w", err)
			}
			tag, err := tx.Exec(ctx, `
UPDATE message_boxes SET media=$3,pts=$4
WHERE owner_user_id=$1 AND box_id=$2 AND NOT deleted`, box.OwnerUserID, box.BoxID, mediaJSON, int32(pts))
			if err != nil {
				return nil, fmt.Errorf("update retired star gift projection: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return nil, fmt.Errorf("update retired star gift projection lost row")
			}
			msg, err := messageFromVisibleBoxRow(box)
			if err != nil {
				return nil, err
			}
			msg.Media = media
			msg.Pts = pts
			if err := replaceMessageBoxMediaIndexTx(ctx, tx, msg.OwnerUserID, msg.Peer.ID, msg.ID, msg.Date, msg.Media, msg.Entities); err != nil {
				return nil, err
			}
			event := domain.UpdateEvent{UserID: msg.OwnerUserID, Type: domain.UpdateEventEditMessage,
				Pts: pts, PtsCount: 1, Date: date, Message: msg}
			if err := appendUserUpdateEvent(ctx, tx, q, msg.OwnerUserID, event); err != nil {
				return nil, fmt.Errorf("append retired star gift edit event: %w", err)
			}
			if err := enqueueDispatch(ctx, q, dispatchEnqueue{
				TargetUserID: msg.OwnerUserID, Pts: int32(pts), EventType: string(domain.UpdateEventEditMessage),
				ExcludeAuthKeyID: [8]byte{}, ExcludeSessionID: 0,
			}); err != nil {
				return nil, fmt.Errorf("enqueue retired star gift edit: %w", err)
			}
			if box.OwnerUserID == box.MessageSenderID || len(privateMediaJSON) == 0 {
				privateMediaJSON, err = encodeSharedPrivateStarGiftMedia(media)
				if err != nil {
					return nil, err
				}
			}
			edits = append(edits, domain.EditedMessageForUser{UserID: msg.OwnerUserID, Message: msg, Event: event})
		}
		if !matched {
			continue
		}
		if len(privateMediaJSON) == 0 {
			return nil, fmt.Errorf("retired star gift projection missing shared media")
		}
		if _, err := tx.Exec(ctx, `
UPDATE private_messages SET media=$3
WHERE sender_user_id=$1 AND id=$2`, target.MessageSenderID, target.PrivateMessageID, privateMediaJSON); err != nil {
			return nil, fmt.Errorf("update retired star gift private media: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM star_gift_user_message_refs
WHERE owner_user_id=$1 AND saved_gift_id=$2`, source.Owner.ID, source.ID); err != nil {
		return nil, fmt.Errorf("delete retired user star gift aliases: %w", err)
	}
	return edits, nil
}

// soldFor is nil for every ownership change except a completed marketplace
// sale, matching retireUserStarGiftMessagesTx's own soldFor parameter.
func (s *StarGiftLifecycleStore) retireChannelStarGiftMessagesTx(
	ctx context.Context,
	tx pgx.Tx,
	soldFor *domain.StarGiftAmount,
	source domain.SavedStarGift,
	current domain.UniqueStarGift,
	refs []starGiftViewerMessageRef,
	date int,
) ([]domain.EditedMessageForUser, error) {
	if s == nil || s.messages == nil || source.Owner.Type != domain.PeerTypeChannel || source.Owner.ID <= 0 ||
		source.ID <= 0 || source.SavedID <= 0 || source.UniqueGiftID <= 0 || current.ID != source.UniqueGiftID || date <= 0 {
		return nil, domain.ErrStarGiftTransferUnavailable
	}
	q := sqlcgen.New(tx)
	edits := make([]domain.EditedMessageForUser, 0, len(refs))
	for _, ref := range refs {
		var messageSenderID, privateMessageID int64
		err := tx.QueryRow(ctx, `
SELECT message_sender_id,private_message_id
FROM message_boxes
WHERE owner_user_id=$1 AND box_id=$2 AND peer_type='user' AND NOT deleted
FOR UPDATE`, ref.UserID, ref.MsgID).Scan(&messageSenderID, &privateMessageID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("lock retired channel star gift projection: %w", err)
		}
		boxes, err := q.ListVisibleMessageBoxesByPrivateMessage(ctx, sqlcgen.ListVisibleMessageBoxesByPrivateMessageParams{
			OwnerUserIds: []int64{ref.UserID}, MessageSenderID: messageSenderID, PrivateMessageID: privateMessageID,
		})
		if err != nil {
			return nil, fmt.Errorf("load retired channel star gift projection: %w", err)
		}
		if len(boxes) != 1 || int(boxes[0].BoxID) != ref.MsgID {
			return nil, domain.ErrStarGiftTransferUnavailable
		}
		box := boxes[0]
		media, err := decodeMessageMedia(box.MediaJson)
		if err != nil {
			return nil, fmt.Errorf("decode retired channel star gift projection: %w", err)
		}
		if media == nil || media.ServiceAction == nil {
			return nil, fmt.Errorf("retired channel star gift projection has no service action")
		}
		switch media.ServiceAction.Kind {
		case domain.MessageServiceActionStarGift:
			action := media.ServiceAction.StarGift
			if action == nil || action.GiftID != source.GiftID || action.PeerChannelID != source.Owner.ID ||
				action.SavedID != source.SavedID {
				return nil, fmt.Errorf("retired channel star gift ordinary projection has invalid identity")
			}
			action.Saved = false
			action.CanUpgrade = false
			action.PrepaidUpgrade = false
			action.PrepaidUpgradeHash = ""
			action.UpgradeStars = 0
			action.UpgradeMsgID = 0
		case domain.MessageServiceActionStarGiftUnique:
			action := media.ServiceAction.StarGiftUnique
			if action == nil || action.Gift.ID != current.ID || action.Peer != source.Owner || action.SavedID != source.SavedID {
				return nil, fmt.Errorf("retired channel star gift unique projection has invalid identity")
			}
			retiredGift := current
			retiredGift.CraftChancePermille = 0
			retiredGift.ResellAmount = nil
			action.Gift = retiredGift
			action.Peer = domain.Peer{}
			action.SavedID = 0
			action.Saved = false
			action.Transferred = true
			action.CanExportAt = 0
			action.TransferStars = 0
			action.ResaleAmount = soldFor
			action.CanTransferAt = 0
			action.CanResellAt = 0
			action.DropOriginalDetailsStars = 0
			action.CanCraftAt = 0
		default:
			return nil, fmt.Errorf("retired channel star gift projection has invalid action")
		}
		mediaJSON, err := encodeMessageMedia(media)
		if err != nil {
			return nil, fmt.Errorf("encode retired channel star gift projection: %w", err)
		}
		pts, err := s.messages.reservePts(ctx, tx, ref.UserID)
		if err != nil {
			return nil, fmt.Errorf("allocate retired channel star gift pts: %w", err)
		}
		tag, err := tx.Exec(ctx, `
UPDATE message_boxes SET media=$3,pts=$4
WHERE owner_user_id=$1 AND box_id=$2 AND NOT deleted`, ref.UserID, ref.MsgID, mediaJSON, int32(pts))
		if err != nil {
			return nil, fmt.Errorf("update retired channel star gift projection: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return nil, fmt.Errorf("update retired channel star gift projection lost row")
		}
		sharedMediaJSON, err := encodeSharedPrivateStarGiftMedia(media)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE private_messages SET media=$3
WHERE sender_user_id=$1 AND id=$2`, messageSenderID, privateMessageID, sharedMediaJSON); err != nil {
			return nil, fmt.Errorf("update retired channel star gift shared media: %w", err)
		}
		message, err := messageFromVisibleBoxRow(box)
		if err != nil {
			return nil, err
		}
		message.Media = media
		message.Pts = pts
		if err := replaceMessageBoxMediaIndexTx(ctx, tx, message.OwnerUserID, message.Peer.ID,
			message.ID, message.Date, message.Media, message.Entities); err != nil {
			return nil, err
		}
		event := domain.UpdateEvent{UserID: ref.UserID, Type: domain.UpdateEventEditMessage,
			Pts: pts, PtsCount: 1, Date: date, Message: message}
		if err := appendUserUpdateEvent(ctx, tx, q, ref.UserID, event); err != nil {
			return nil, fmt.Errorf("append retired channel star gift edit event: %w", err)
		}
		if err := enqueueDispatch(ctx, q, dispatchEnqueue{
			TargetUserID: ref.UserID, Pts: int32(pts), EventType: string(domain.UpdateEventEditMessage),
			ExcludeAuthKeyID: [8]byte{}, ExcludeSessionID: 0,
		}); err != nil {
			return nil, fmt.Errorf("enqueue retired channel star gift edit: %w", err)
		}
		edits = append(edits, domain.EditedMessageForUser{UserID: ref.UserID, Message: message, Event: event})
	}
	if len(refs) > 0 {
		userIDs := make([]int64, 0, len(refs))
		messageIDs := make([]int32, 0, len(refs))
		for _, ref := range refs {
			userIDs = append(userIDs, ref.UserID)
			messageIDs = append(messageIDs, int32(ref.MsgID))
		}
		if _, err := tx.Exec(ctx, `DELETE FROM star_gift_user_message_refs ref
WHERE ref.saved_gift_id=$1 AND (ref.owner_user_id,ref.msg_id) IN (
    SELECT owner_user_id,msg_id FROM unnest($2::bigint[],$3::integer[]) AS retired(owner_user_id,msg_id)
)`, source.ID, userIDs, messageIDs); err != nil {
			return nil, fmt.Errorf("delete retired channel star gift aliases: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM star_gift_channel_notification_jobs
WHERE saved_gift_id=$1 AND (action #>> '{peer_channel_id}')::bigint=$2`, source.ID, source.Owner.ID); err != nil {
		return nil, fmt.Errorf("delete retired channel star gift notification jobs: %w", err)
	}
	return edits, nil
}
