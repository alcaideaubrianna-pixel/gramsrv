package store

import (
	"context"
	"fmt"

	"telesrv/internal/domain"
)

// UsernameRegistryStore reads a peer's full username list. The list is the
// projection source for the TL usernames vector, so every caller sees the same
// stored order the client will render. Reorder may promote a collectible ahead
// of the editable slot; the first active row is also the Layer 228 main scalar.
type UsernameRegistryStore interface {
	// PeerUsernames returns the peer's registry rows in projection order.
	PeerUsernames(ctx context.Context, peer domain.Peer) ([]domain.Username, error)
	// PeerUsernamesBatch resolves several peers in one round trip; peers absent
	// from the result simply hold no usernames.
	PeerUsernamesBatch(ctx context.Context, peers []domain.Peer) (map[domain.Peer][]domain.Username, error)
	// SetUsernameActive toggles one collectible row. It never touches the
	// editable slot and returns domain.ErrUsernameNotCollectible if asked to.
	SetUsernameActive(ctx context.Context, peer domain.Peer, username string, active bool) (bool, error)
	// ReorderUsernames rewrites collectible sort order. order must be a
	// permutation of the peer's collectible usernames.
	ReorderUsernames(ctx context.Context, peer domain.Peer, order []string) (bool, error)
	// DeactivateAllUsernames clears the active flag on every collectible row and
	// reports whether anything changed. The editable slot is left alone.
	DeactivateAllUsernames(ctx context.Context, peer domain.Peer) (bool, error)
}

// CollectibleUsernameStore owns the collectible asset lifecycle: minting into the
// operator vault, assigning to a holder, returning to the vault and burning.
//
// Every mutation is expected to be atomic with the corresponding username
// registry row, so an asset can never be owned without being resolvable and can
// never be resolvable without being owned.
type CollectibleUsernameStore interface {
	// MintCollectibleUsername creates the asset. A request carrying an owner
	// assigns it in the same transaction. Replaying the same CommandKey returns
	// the original asset and reports created=false.
	MintCollectibleUsername(ctx context.Context, req domain.MintCollectibleUsernameRequest) (asset domain.CollectibleUsername, created bool, err error)
	// TransferCollectibleUsername moves the asset to req.To, out of the vault or
	// from the current holder. Replay by CommandKey is a no-op returning the
	// current state with changed=false.
	TransferCollectibleUsername(ctx context.Context, req domain.TransferCollectibleUsernameRequest) (asset domain.CollectibleUsername, changed bool, err error)
	// PurchaseCollectibleUsername is the public, self-service buy: it debits the
	// buyer's balance at the asset's recorded price and moves it out of the
	// vault. It returns domain.ErrCollectibleUsernameNotForSale for anything not
	// vault-held or unpriced, and domain.ErrStarsInsufficient on a short balance.
	PurchaseCollectibleUsername(ctx context.Context, req domain.PurchaseCollectibleUsernameRequest) (asset domain.CollectibleUsername, err error)
	// RevokeCollectibleUsername returns the asset to the vault, or burns it when
	// req.Burn is set. Burning releases the name back to the free pool.
	RevokeCollectibleUsername(ctx context.Context, req domain.RevokeCollectibleUsernameRequest) (asset domain.CollectibleUsername, changed bool, err error)
	// DeleteCollectibleUsername removes the live asset for a name completely --
	// registry row, asset and provenance -- and frees the name for any use. A
	// replay finds nothing live left and reports deleted=false without an error,
	// because the record a command key would key on is gone.
	DeleteCollectibleUsername(ctx context.Context, req domain.DeleteCollectibleUsernameRequest) (deleted bool, err error)
	// CollectibleUsername looks the asset up by name.
	CollectibleUsername(ctx context.Context, username string) (domain.CollectibleUsername, error)
	// CollectibleUsernameByID looks the asset up by identity.
	CollectibleUsernameByID(ctx context.Context, id int64) (domain.CollectibleUsername, error)
	// ListCollectibleUsernames is the admin listing query with keyset paging.
	ListCollectibleUsernames(ctx context.Context, filter domain.CollectibleUsernameFilter) ([]domain.CollectibleUsername, error)
	// CollectibleUsernameTransfers returns the provenance log, newest first.
	CollectibleUsernameTransfers(ctx context.Context, collectibleID int64, limit int) ([]domain.CollectibleUsernameTransfer, error)
}

// UsernameAudienceDeliverySnapshot freezes every affected user peer and its
// bounded known-viewer audience while the username aggregate transaction is
// still open. Channel peers are intentionally absent: their delivery belongs to
// the channel aggregate/order domain.
type UsernameAudienceDeliverySnapshot struct {
	Users []UserAudienceDeliverySnapshot
}

type UsernameRegistryDeliveryStore interface {
	SetUsernameActiveWithDelivery(ctx context.Context, peer domain.Peer, username string, active bool, effects DeliveryEffectsBuilder[UsernameAudienceDeliverySnapshot]) (bool, error)
	ReorderUsernamesWithDelivery(ctx context.Context, peer domain.Peer, order []string, effects DeliveryEffectsBuilder[UsernameAudienceDeliverySnapshot]) (bool, error)
	DeactivateAllUsernamesWithDelivery(ctx context.Context, peer domain.Peer, effects DeliveryEffectsBuilder[UsernameAudienceDeliverySnapshot]) (bool, error)
}

type CollectibleUsernameDeliveryStore interface {
	MintCollectibleUsernameWithDelivery(ctx context.Context, req domain.MintCollectibleUsernameRequest, effects DeliveryEffectsBuilder[UsernameAudienceDeliverySnapshot]) (asset domain.CollectibleUsername, created bool, err error)
	UpdateCollectibleUsernamePriceWithDelivery(ctx context.Context, req domain.UpdateCollectibleUsernamePriceRequest, effects DeliveryEffectsBuilder[UsernameAudienceDeliverySnapshot]) (asset domain.CollectibleUsername, changed bool, err error)
	TransferCollectibleUsernameWithDelivery(ctx context.Context, req domain.TransferCollectibleUsernameRequest, effects DeliveryEffectsBuilder[UsernameAudienceDeliverySnapshot]) (asset domain.CollectibleUsername, changed bool, err error)
	PurchaseCollectibleUsernameWithDelivery(ctx context.Context, req domain.PurchaseCollectibleUsernameRequest, effects DeliveryEffectsBuilder[UsernameAudienceDeliverySnapshot]) (asset domain.CollectibleUsername, err error)
	RevokeCollectibleUsernameWithDelivery(ctx context.Context, req domain.RevokeCollectibleUsernameRequest, effects DeliveryEffectsBuilder[UsernameAudienceDeliverySnapshot]) (asset domain.CollectibleUsername, changed bool, err error)
	DeleteCollectibleUsernameWithDelivery(ctx context.Context, req domain.DeleteCollectibleUsernameRequest, effects DeliveryEffectsBuilder[UsernameAudienceDeliverySnapshot]) (deleted bool, err error)
}

func ValidateUsernameAudienceDeliveryEffects(snapshot UsernameAudienceDeliverySnapshot, effects []DeliveryEffect) error {
	want := make(map[int64]int)
	expected := 0
	seenUsers := make(map[int64]struct{}, len(snapshot.Users))
	for index, user := range snapshot.Users {
		if user.User.ID <= 0 {
			return fmt.Errorf("username delivery snapshot %d has invalid user", index)
		}
		if _, duplicate := seenUsers[user.User.ID]; duplicate {
			return fmt.Errorf("username delivery snapshot duplicates user %d", user.User.ID)
		}
		seenUsers[user.User.ID] = struct{}{}
		seenAudience := make(map[int64]struct{}, len(user.Audience))
		for _, viewerID := range user.Audience {
			if viewerID <= 0 {
				return fmt.Errorf("username delivery snapshot contains invalid viewer")
			}
			if _, duplicate := seenAudience[viewerID]; duplicate {
				return fmt.Errorf("username delivery snapshot duplicates viewer %d", viewerID)
			}
			seenAudience[viewerID] = struct{}{}
			want[viewerID]++
			expected++
		}
	}
	if len(effects) != expected {
		return fmt.Errorf("username delivery requires %d effects, got %d", expected, len(effects))
	}
	got := make(map[int64]int, len(want))
	for index := range effects {
		effect := effects[index]
		if err := effect.Validate(); err != nil {
			return fmt.Errorf("username delivery effect %d: %w", index, err)
		}
		if effect.Kind != DeliveryEffectAbsolute || want[effect.TargetUserID] == 0 {
			return fmt.Errorf("username delivery effect %d has unexpected target", index)
		}
		got[effect.TargetUserID]++
	}
	for target, count := range want {
		if got[target] != count {
			return fmt.Errorf("username delivery target %d count %d, want %d", target, got[target], count)
		}
	}
	return nil
}
