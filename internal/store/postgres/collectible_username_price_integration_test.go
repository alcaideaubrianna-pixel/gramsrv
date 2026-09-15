package postgres

import (
	"context"
	"errors"
	"testing"

	"telesrv/internal/domain"
)

func TestUpdateCollectibleUsernamePricePostgres(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	suffix := randomSuffix(t)
	registry := NewCollectibleUsernameStore(pool)
	username := "reprice" + suffix
	seed, created, err := registry.MintCollectibleUsername(ctx, mintRequest(username, domain.Peer{}, "mint-"+suffix))
	if err != nil || !created || seed.Currency != domain.CollectibleCurrencyStars || seed.Amount != 5000 {
		t.Fatalf("seed vault asset = %+v created:%v err:%v", seed, created, err)
	}

	updated, changed, err := registry.UpdateCollectibleUsernamePriceWithDelivery(ctx, domain.UpdateCollectibleUsernamePriceRequest{
		Username: username, Currency: domain.CollectibleCurrencyTON, Amount: 2_000_000_000, Actor: "ops", Reason: "reprice",
	}, purchaseUsernameDelivery)
	if err != nil || !changed || updated.Currency != domain.CollectibleCurrencyTON || updated.Amount != 2_000_000_000 {
		t.Fatalf("update = %+v changed:%v err:%v", updated, changed, err)
	}
	if updated.Status != domain.CollectibleUsernameStatusVault || updated.Version != seed.Version+1 {
		t.Fatalf("update side effects unexpected: %+v", updated)
	}

	// A no-op update (same price) reports changed=false.
	same, changed, err := registry.UpdateCollectibleUsernamePriceWithDelivery(ctx, domain.UpdateCollectibleUsernamePriceRequest{
		Username: username, Currency: domain.CollectibleCurrencyTON, Amount: 2_000_000_000,
	}, purchaseUsernameDelivery)
	if err != nil || changed || same.Version != updated.Version {
		t.Fatalf("no-op update = %+v changed:%v err:%v", same, changed, err)
	}

	// A buyer can now only afford it at the new (TON) price.
	buyer := collectibleTestUser(t, pool, 9_500_000+int64(len(suffix)), "repricebuyer"+suffix)
	if _, err := pool.Exec(ctx, `INSERT INTO ton_balances(user_id,balance_nanoton,granted) VALUES($1,2000000000,true)`, buyer.ID); err != nil {
		t.Fatalf("seed buyer ton balance: %v", err)
	}
	bought, err := registry.PurchaseCollectibleUsernameWithDelivery(ctx, domain.PurchaseCollectibleUsernameRequest{
		Username: username, Buyer: buyer, CommandKey: "buy-" + suffix,
	}, purchaseUsernameDelivery)
	if err != nil || bought.Owner != buyer {
		t.Fatalf("purchase at repriced value = %+v err=%v", bought, err)
	}

	// Burned assets refuse a reprice.
	burnedName := "burnedreprice" + suffix
	if _, created, err := registry.MintCollectibleUsername(ctx, mintRequest(burnedName, domain.Peer{}, "mint2-"+suffix)); err != nil || !created {
		t.Fatalf("seed burn target created:%v err:%v", created, err)
	}
	if _, _, err := registry.RevokeCollectibleUsername(ctx, domain.RevokeCollectibleUsernameRequest{
		Username: burnedName, Burn: true, CommandKey: "burn-" + suffix,
	}); err != nil {
		t.Fatalf("burn: %v", err)
	}
	if _, _, err := registry.UpdateCollectibleUsernamePriceWithDelivery(ctx, domain.UpdateCollectibleUsernamePriceRequest{
		Username: burnedName, Currency: domain.CollectibleCurrencyStars, Amount: 1,
	}, purchaseUsernameDelivery); !errors.Is(err, domain.ErrCollectibleUsernameBurned) {
		t.Fatalf("err = %v, want ErrCollectibleUsernameBurned", err)
	}
}
