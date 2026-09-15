package rpc

import (
	"context"
	"testing"

	"github.com/iamxvbaba/td/bin"
	"github.com/iamxvbaba/td/clock"
	"github.com/iamxvbaba/td/tg"
	"go.uber.org/zap/zaptest"

	"telesrv/internal/domain"
)

type resaleAttributesRPCService struct {
	GiftsService
	preview domain.StarGiftUpgradePreview
}

func (s *resaleAttributesRPCService) ListResale(context.Context, domain.StarGiftResaleFilter) (domain.StarGiftResalePage, error) {
	return domain.StarGiftResalePage{}, nil
}

func (s *resaleAttributesRPCService) CollectiblePreview(context.Context, int64) (domain.StarGiftUpgradePreview, bool, error) {
	return s.preview, true, nil
}

// TestResaleStarGiftsAttributesAlwaysNonNilWhenFlagSet covers the exact bug
// found live in production: PaymentsResaleStarGifts.Attributes and
// .AttributesHash share ONE wire flag bit (flags.1) in the real Telegram TL
// schema. Calling only SetAttributesHash (the "client's cached attributes_hash
// already matches, nothing to resend" case) sets that bit while leaving
// Attributes at its Go zero value (nil) -- the TL encoder then rejects EVERY
// such response with "malformed canonical value: explicit flag has nil
// interface field attributes", permanently stalling the resale gifts screen
// for real users. Both branches (hash matches / hash differs) must leave
// Attributes non-nil whenever the flag ends up set.
func TestResaleStarGiftsAttributesAlwaysNonNilWhenFlagSet(t *testing.T) {
	preview := domain.StarGiftUpgradePreview{
		GiftID:    9000000000000001,
		Revision:  7,
		Models:    []domain.StarGiftCollectibleAttribute{{Name: "Classic"}},
		Patterns:  []domain.StarGiftCollectibleAttribute{{Name: "Dots"}},
		Backdrops: []domain.StarGiftCollectibleAttribute{{Name: "Blue"}},
	}
	svc := &resaleAttributesRPCService{preview: preview}
	r := New(Config{DC: 2}, Deps{Gifts: svc}, zaptest.NewLogger(t), clock.System)

	for _, tc := range []struct {
		name          string
		clientHash    int64
		wantAttrCount int
	}{
		{"hash_mismatch_sends_full_attributes", 0, 3},
		{"hash_matches_sends_empty_but_non_nil", 7, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &tg.PaymentsGetResaleStarGiftsRequest{GiftID: 9000000000000001, Limit: 10}
			req.SetAttributesHash(tc.clientHash)

			resp, err := r.onPaymentsGetResaleStarGifts(context.Background(), req)
			if err != nil {
				t.Fatalf("onPaymentsGetResaleStarGifts: %v", err)
			}

			attrs, ok := resp.GetAttributes()
			if !ok {
				t.Fatalf("Attributes flag not set at all -- AttributesHash/Attributes flag desynced")
			}
			if attrs == nil {
				t.Fatalf("flag says Attributes present, but the slice is nil -- this is exactly the live encoder bug")
			}
			if len(attrs) != tc.wantAttrCount {
				t.Fatalf("len(attrs) = %d, want %d", len(attrs), tc.wantAttrCount)
			}

			// Belt-and-suspenders: also round-trip through the plain TL
			// encoder (the live bug was in the *layer profile* encoder used
			// for older client layers, but a self-inconsistent struct is
			// worth catching here too).
			buf := &bin.Buffer{}
			if err := resp.Encode(buf); err != nil {
				t.Fatalf("Encode: %v", err)
			}
		})
	}
}
