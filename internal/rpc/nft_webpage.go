package rpc

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"telesrv/internal/branding"
	"telesrv/internal/domain"
)

// nftGiftWebPageType is the client-recognized webpage.type that switches
// ChatMessageCell (and the desktop/other client forks) from the generic
// link-preview card to the native "collectible gift" card -- backdrop
// artwork, VIEW COLLECTIBLE button, Model/Backdrop/Symbol lines -- exactly
// like real Telegram's t.me/nft/<slug> links. It only fires when the
// message also carries a webPageAttributeUniqueStarGift attribute wrapping
// a real starGiftUnique; see ChatMessageCell.java's "telegram_nft" branch.
const nftGiftWebPageType = "telegram_nft"

// resolveUniqueGiftWebPage synthesizes a structured link-preview card for
// our own /nft/{slug} links directly from the catalog, mirroring
// resolveAIComposeStyleWebPage's own-domain short-circuit. The generic
// HTML-scraping fetcher in app/files can still produce a valid og:image
// card for external tools (Discord, browsers, ...) that hit the public
// page directly; this path exists so *our own client* renders the same
// rich native gift card real Telegram does, which requires a real TL
// starGiftUnique attribute that no amount of HTML meta tags can carry.
func (r *Router) resolveUniqueGiftWebPage(ctx context.Context, rawURL string) (domain.MessageWebPage, bool) {
	link, ok := parseNFTGiftLink(rawURL, r.publicLinkHost())
	if !ok || r.deps.Gifts == nil {
		return domain.MessageWebPage{}, false
	}
	unique, found, err := r.deps.Gifts.UniqueBySlug(ctx, link.slug)
	if err != nil || !found || unique.Num <= 0 {
		return domain.MessageWebPage{}, false
	}

	now := time.Now()
	if r.clock != nil {
		now = r.clock.Now()
	}
	title := strings.TrimSpace(unique.Title)
	if title == "" {
		title = "Collectible gift"
	}
	page := domain.MessageWebPage{
		State:      domain.MessageWebPageStateDone,
		ID:         domain.WebPageURLHash(link.normalized),
		URL:        link.normalized,
		DisplayURL: link.display,
		Hash:       nftGiftWebPageHash(unique),
		Date:       int(now.Unix()),
		Type:       nftGiftWebPageType,
		SiteName:   branding.ProductName(),
		Title:      title,
		Description: fmt.Sprintf("Collectible #%d on %s. Open %s to view its current details.",
			unique.Num, branding.ProductName(), branding.ProductName()),
		UniqueGift: &unique,
	}
	if r.deps.Files != nil {
		if photo, err := r.deps.Files.CreatePhotoFromURL(ctx, nftPreviewImageURL(r.cfg.PublicBaseURL, link.slug)); err == nil {
			page.Photo = &photo
			page.HasLargeMedia = true
		}
		// Best-effort: an unreachable/slow-to-render-for-the-first-time
		// preview image must not block the card itself from showing --
		// the client's native "telegram_nft" branch draws its own
		// backdrop/model art from the attribute regardless of Photo.
	}
	return page, true
}

func nftPreviewImageURL(publicBaseURL, slug string) string {
	return strings.TrimRight(publicBaseURL, "/") + "/_public/gift-preview/" + url.PathEscape(slug) + ".png"
}

type nftGiftLink struct {
	normalized string
	display    string
	slug       string
}

func parseNFTGiftLink(raw, publicHost string) (nftGiftLink, bool) {
	normalized, ok := domain.NormalizeWebPageURL(raw)
	if !ok {
		return nftGiftLink{}, false
	}
	u, err := url.Parse(normalized)
	if err != nil {
		return nftGiftLink{}, false
	}
	host := strings.ToLower(u.Hostname())
	if !aiComposeStyleHostAllowed(host, publicHost) {
		// Reuses the same own-domain allowlist (public host + t.me/telegram.me
		// + local dev hosts) as the AI compose style resolver; there is
		// nothing AI-compose-specific about that check.
		return nftGiftLink{}, false
	}
	parts := strings.Split(strings.Trim(strings.ToLower(u.EscapedPath()), "/"), "/")
	if len(parts) != 2 || parts[0] != "nft" {
		return nftGiftLink{}, false
	}
	slug, err := url.PathUnescape(parts[1])
	if err != nil {
		return nftGiftLink{}, false
	}
	slug = strings.ToLower(strings.TrimSpace(slug))
	if !validNFTGiftSlug(slug) {
		return nftGiftLink{}, false
	}
	display := host + "/nft/" + slug
	return nftGiftLink{normalized: normalized, display: display, slug: slug}, true
}

// validNFTGiftSlug mirrors the public Web handler's slug shape check
// (internal/web's validStarGiftSlugPath) without importing that package:
// ASCII letters/digits/hyphens, first char alphabetic, no run of consecutive
// hyphens, reasonable length bound.
func validNFTGiftSlug(slug string) bool {
	if len(slug) == 0 || len(slug) > 64 {
		return false
	}
	if c := slug[0]; !(c >= 'a' && c <= 'z') {
		return false
	}
	prevHyphen := false
	for i := 0; i < len(slug); i++ {
		c := slug[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			prevHyphen = false
		case c == '-':
			if prevHyphen {
				return false
			}
			prevHyphen = true
		default:
			return false
		}
	}
	return !prevHyphen
}

func nftGiftWebPageHash(unique domain.UniqueStarGift) int {
	// Unique gifts are immutable once minted (model/pattern/backdrop/number
	// never change), so a constant per-gift value is enough for the
	// client's "did this webpage change" comparison -- fold in the id so
	// distinct gifts still get distinct hashes.
	return int(unique.ID & 0x7fffffff)
}
