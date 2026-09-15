package rpc

import (
	"context"
	"testing"

	"github.com/iamxvbaba/td/clock"
	"github.com/iamxvbaba/td/tg"
	"go.uber.org/zap/zaptest"

	"telesrv/internal/domain"
)

// TestResolveUniqueGiftWebPageProducesNativeGiftCard verifies the end-to-end
// synthesis that lets gifts.example.test/nft/{slug} links render as Telegram's native
// "collectible gift" card instead of a generic link preview: the resolved
// domain.MessageWebPage must carry webpage.type=="telegram_nft" plus a
// populated UniqueGift, and the tg.WebPage conversion must attach a real
// WebPageAttributeUniqueStarGift wrapping the exact same gift -- that
// attribute is what ChatMessageCell keys its native rendering off of.
func TestResolveUniqueGiftWebPageProducesNativeGiftCard(t *testing.T) {
	unique := domain.UniqueStarGift{
		ID:       555,
		GiftID:   20,
		Title:    "Algorithm Cup",
		Slug:     "algorithm-cup-20",
		Num:      20,
		Model:    domain.StarGiftCollectibleAttribute{Name: "Classic"},
		Pattern:  domain.StarGiftCollectibleAttribute{Name: "Dots"},
		Backdrop: domain.StarGiftCollectibleAttribute{Name: "Blue"},
	}
	gifts := &uniqueGiftRPCService{unique: unique}
	files := &fakeFiles{}
	r := New(Config{DC: 2, PublicBaseURL: "https://gifts.example.test"}, Deps{Gifts: gifts, Files: files}, zaptest.NewLogger(t), clock.System)

	for _, raw := range []string{
		"https://gifts.example.test/nft/algorithm-cup-20",
		"HTTPS://GIFTS.EXAMPLE.TEST/nft/Algorithm-Cup-20",
	} {
		t.Run(raw, func(t *testing.T) {
			page, ok := r.resolveUniqueGiftWebPage(context.Background(), raw)
			if !ok {
				t.Fatalf("resolveUniqueGiftWebPage(%q) ok=false, want true", raw)
			}
			if page.Type != nftGiftWebPageType {
				t.Fatalf("page.Type = %q, want %q", page.Type, nftGiftWebPageType)
			}
			if page.UniqueGift == nil || page.UniqueGift.Slug != unique.Slug || page.UniqueGift.Num != unique.Num {
				t.Fatalf("page.UniqueGift = %+v, want a copy of %+v", page.UniqueGift, unique)
			}
			if page.Photo == nil {
				t.Fatalf("page.Photo = nil, want a preview photo synthesized from the render endpoint")
			}

			tgPage := tgWebPage(page)
			wp, ok := tgPage.(*tg.WebPage)
			if !ok {
				t.Fatalf("tgWebPage() = %T, want *tg.WebPage", tgPage)
			}
			attrs, _ := wp.GetAttributes()
			var found *tg.WebPageAttributeUniqueStarGift
			for _, a := range attrs {
				if u, ok := a.(*tg.WebPageAttributeUniqueStarGift); ok {
					found = u
				}
			}
			if found == nil {
				t.Fatalf("wp.Attributes = %+v, want a WebPageAttributeUniqueStarGift", attrs)
			}
			giftUnique, ok := found.Gift.(*tg.StarGiftUnique)
			if !ok {
				t.Fatalf("attribute.Gift = %T, want *tg.StarGiftUnique", found.Gift)
			}
			if giftUnique.Slug != unique.Slug {
				t.Errorf("attribute gift slug = %q, want %q", giftUnique.Slug, unique.Slug)
			}
			if giftUnique.Num != unique.Num {
				t.Errorf("attribute gift num = %d, want %d", giftUnique.Num, unique.Num)
			}
		})
	}
}

// TestResolveUniqueGiftWebPageRejectsForeignHostsAndUnknownSlugs makes sure
// the short-circuit stays scoped to our own domain and to gifts that
// actually exist, so it never shadows the generic HTML-scraping fetcher for
// unrelated links.
func TestResolveUniqueGiftWebPageRejectsForeignHostsAndUnknownSlugs(t *testing.T) {
	unique := domain.UniqueStarGift{ID: 1, Title: "X", Slug: "algorithm-cup-20", Num: 20}
	gifts := &uniqueGiftRPCService{unique: unique}
	r := New(Config{DC: 2, PublicBaseURL: "https://gifts.example.test"}, Deps{Gifts: gifts, Files: &fakeFiles{}}, zaptest.NewLogger(t), clock.System)

	for _, raw := range []string{
		"https://evil.example/nft/algorithm-cup-20", // not our own public host (nor the shared aicompose allowlist)
		"https://gifts.example.test/nft/does-not-exist",         // valid shape, unknown slug
		"https://gifts.example.test/nft/algorithm-cup-20/foo",   // extra path segment
		"https://gifts.example.test/username/algorithm-cup-20",
	} {
		if _, ok := r.resolveUniqueGiftWebPage(context.Background(), raw); ok {
			t.Errorf("resolveUniqueGiftWebPage(%q) ok=true, want false", raw)
		}
	}
}
