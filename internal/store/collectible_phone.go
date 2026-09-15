package store

import (
	"context"
	"fmt"
	"telesrv/internal/domain"
)

type CollectiblePhoneStore interface {
	CollectiblePhone(context.Context, string) (domain.CollectiblePhone, error)
	CollectiblePhoneByID(context.Context, int64) (domain.CollectiblePhone, error)
	OwnedCollectiblePhones(context.Context, []int64) (map[int64]domain.CollectiblePhone, error)
	ListCollectiblePhones(context.Context, domain.CollectiblePhoneFilter) ([]domain.CollectiblePhone, error)
	CollectiblePhoneTransfers(context.Context, int64, int) ([]domain.CollectiblePhoneTransfer, error)
}

// CollectiblePhoneDeliverySnapshot freezes the changed asset plus the exact
// user/viewer projections affected by its owner transition. The store builds
// this snapshot before committing the phone aggregate; callers must return one
// absolute updateUser effect for every frozen viewer cell.
type CollectiblePhoneDeliverySnapshot struct {
	Asset domain.CollectiblePhone
	Users []UserAudienceDeliverySnapshot
}

type CollectiblePhoneDeliveryStore interface {
	CollectiblePhoneStore
	MintCollectiblePhoneWithDelivery(context.Context, domain.MintCollectiblePhoneRequest, DeliveryEffectsBuilder[CollectiblePhoneDeliverySnapshot]) (domain.CollectiblePhone, bool, error)
	UpdateCollectiblePhonePriceWithDelivery(context.Context, domain.UpdateCollectiblePhonePriceRequest, DeliveryEffectsBuilder[CollectiblePhoneDeliverySnapshot]) (domain.CollectiblePhone, bool, error)
	TransferCollectiblePhoneWithDelivery(context.Context, domain.TransferCollectiblePhoneRequest, DeliveryEffectsBuilder[CollectiblePhoneDeliverySnapshot]) (domain.CollectiblePhone, bool, error)
	// PurchaseCollectiblePhoneWithDelivery is the public, self-service buy: it
	// debits the buyer's TON balance at the asset's recorded CryptoAmount price
	// and moves it out of the vault. It returns domain.ErrCollectiblePhoneNotForSale
	// for anything not vault-held, and domain.ErrStarsInsufficient on a short balance.
	PurchaseCollectiblePhoneWithDelivery(context.Context, domain.PurchaseCollectiblePhoneRequest, DeliveryEffectsBuilder[CollectiblePhoneDeliverySnapshot]) (domain.CollectiblePhone, bool, error)
	RevokeCollectiblePhoneWithDelivery(context.Context, domain.RevokeCollectiblePhoneRequest, DeliveryEffectsBuilder[CollectiblePhoneDeliverySnapshot]) (domain.CollectiblePhone, bool, error)
	DeleteCollectiblePhoneWithDelivery(context.Context, domain.DeleteCollectiblePhoneRequest, DeliveryEffectsBuilder[CollectiblePhoneDeliverySnapshot]) (bool, error)
}

func ValidateCollectiblePhoneDeliveryEffects(snapshot CollectiblePhoneDeliverySnapshot, effects []DeliveryEffect) error {
	want := make(map[int64]int)
	expected := 0
	seenUsers := make(map[int64]struct{}, len(snapshot.Users))
	for index, user := range snapshot.Users {
		if user.User.ID <= 0 {
			return fmt.Errorf("collectible phone delivery snapshot %d has invalid user", index)
		}
		if _, duplicate := seenUsers[user.User.ID]; duplicate {
			return fmt.Errorf("collectible phone delivery snapshot duplicates user %d", user.User.ID)
		}
		seenUsers[user.User.ID] = struct{}{}
		seenAudience := make(map[int64]struct{}, len(user.Audience))
		for _, viewerID := range user.Audience {
			if viewerID <= 0 {
				return fmt.Errorf("collectible phone delivery snapshot contains invalid viewer")
			}
			if _, duplicate := seenAudience[viewerID]; duplicate {
				return fmt.Errorf("collectible phone delivery snapshot duplicates viewer %d", viewerID)
			}
			seenAudience[viewerID] = struct{}{}
			want[viewerID]++
			expected++
		}
	}
	if len(effects) != expected {
		return fmt.Errorf("collectible phone delivery requires %d effects, got %d", expected, len(effects))
	}
	got := make(map[int64]int, len(want))
	for index := range effects {
		effect := effects[index]
		if err := effect.Validate(); err != nil {
			return fmt.Errorf("collectible phone delivery effect %d: %w", index, err)
		}
		if effect.Kind != DeliveryEffectAbsolute || want[effect.TargetUserID] == 0 {
			return fmt.Errorf("collectible phone delivery effect %d has unexpected target", index)
		}
		if effect.ExcludeAuthKeyID != ([8]byte{}) || effect.ExcludeSessionID != 0 {
			return fmt.Errorf("collectible phone delivery effect %d must not exclude a session", index)
		}
		got[effect.TargetUserID]++
	}
	for target, count := range want {
		if got[target] != count {
			return fmt.Errorf("collectible phone delivery target %d count %d, want %d", target, got[target], count)
		}
	}
	return nil
}
