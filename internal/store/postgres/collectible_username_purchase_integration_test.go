package postgres

import (
	"context"
	"errors"
	"testing"

	"telesrv/internal/domain"
	storepkg "telesrv/internal/store"
)

// purchaseUsernameDelivery emits one absolute effect per frozen (user,viewer)
// pair -- the minimal builder ValidateUsernameAudienceDeliveryEffects accepts.
// These tests only care about the ledger and ownership side effects; the
// audience-freezing/bounding plumbing itself is already proven by
// TestCollectibleUsernameTransferDeliveryAtomicBoundedAndIdempotentPostgres.
func purchaseUsernameDelivery(snapshot storepkg.UsernameAudienceDeliverySnapshot) ([]storepkg.DeliveryEffect, error) {
	effects := make([]storepkg.DeliveryEffect, 0)
	for _, user := range snapshot.Users {
		for _, viewerID := range user.Audience {
			effects = append(effects, storepkg.AbsoluteDeliveryEffect(storepkg.DeliveryOutboxEnqueue{
				TargetUserID:   viewerID,
				Payload:        []byte("collectible-username-purchase"),
				RecoveryPolicy: storepkg.OutboxRecoveryAbsoluteReload,
			}))
		}
	}
	return effects, nil
}

func TestPurchaseCollectibleUsernameStarsPostgres(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	suffix := randomSuffix(t)
	buyer := collectibleTestUser(t, pool, 9_100_000+int64(len(suffix)), "buyer"+suffix)
	if _, err := pool.Exec(ctx, `INSERT INTO stars_balances(user_id,balance,granted) VALUES($1,5000,true)`, buyer.ID); err != nil {
		t.Fatalf("seed buyer stars balance: %v", err)
	}

	registry := NewCollectibleUsernameStore(pool)
	username := "buyme" + suffix
	seed, created, err := registry.MintCollectibleUsername(ctx, mintRequest(username, domain.Peer{}, "mint-"+suffix))
	if err != nil || !created || seed.Status != domain.CollectibleUsernameStatusVault {
		t.Fatalf("seed vault asset = %+v created:%v err:%v", seed, created, err)
	}

	bought, err := registry.PurchaseCollectibleUsernameWithDelivery(ctx, domain.PurchaseCollectibleUsernameRequest{
		Username: username, Buyer: buyer, Actor: "buyer", Reason: "public purchase", CommandKey: "buy-" + suffix,
	}, purchaseUsernameDelivery)
	if err != nil {
		t.Fatalf("purchase: %v", err)
	}
	if bought.Status != domain.CollectibleUsernameStatusOwned || bought.Owner != buyer || bought.OriginalOwner != buyer {
		t.Fatalf("bought asset = %+v", bought)
	}
	if bought.PurchaseDate.Before(seed.PurchaseDate) {
		t.Fatalf("purchase date did not move forward: seed=%v bought=%v", seed.PurchaseDate, bought.PurchaseDate)
	}

	var balance int64
	if err := pool.QueryRow(ctx, `SELECT balance FROM stars_balances WHERE user_id=$1`, buyer.ID).Scan(&balance); err != nil {
		t.Fatalf("read buyer balance: %v", err)
	}
	if balance != 0 {
		t.Fatalf("buyer balance = %d, want 0 (5000 debited)", balance)
	}
	var reason string
	var amount int64
	if err := pool.QueryRow(ctx, `SELECT reason, amount FROM stars_transactions WHERE user_id=$1 ORDER BY id DESC LIMIT 1`, buyer.ID).
		Scan(&reason, &amount); err != nil {
		t.Fatalf("read stars transaction: %v", err)
	}
	if reason != string(domain.StarsReasonCollectible) || amount != -5000 {
		t.Fatalf("stars transaction reason=%q amount=%d", reason, amount)
	}

	// Replaying the same command key must not charge the buyer twice.
	replay, err := registry.PurchaseCollectibleUsernameWithDelivery(ctx, domain.PurchaseCollectibleUsernameRequest{
		Username: username, Buyer: buyer, Actor: "buyer", Reason: "public purchase", CommandKey: "buy-" + suffix,
	}, purchaseUsernameDelivery)
	if err != nil || replay.ID != bought.ID {
		t.Fatalf("replay purchase = %+v err=%v", replay, err)
	}
	if err := pool.QueryRow(ctx, `SELECT balance FROM stars_balances WHERE user_id=$1`, buyer.ID).Scan(&balance); err != nil {
		t.Fatalf("read buyer balance after replay: %v", err)
	}
	if balance != 0 {
		t.Fatalf("replay double-charged: balance = %d, want 0", balance)
	}
}

func TestPurchaseCollectibleUsernameInsufficientStarsPostgres(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	suffix := randomSuffix(t)
	buyer := collectibleTestUser(t, pool, 9_200_000+int64(len(suffix)), "poor"+suffix)
	if _, err := pool.Exec(ctx, `INSERT INTO stars_balances(user_id,balance,granted) VALUES($1,10,true)`, buyer.ID); err != nil {
		t.Fatalf("seed buyer stars balance: %v", err)
	}

	registry := NewCollectibleUsernameStore(pool)
	username := "toopoor" + suffix
	if _, created, err := registry.MintCollectibleUsername(ctx, mintRequest(username, domain.Peer{}, "mint-"+suffix)); err != nil || !created {
		t.Fatalf("seed vault asset created:%v err:%v", created, err)
	}

	if _, err := registry.PurchaseCollectibleUsernameWithDelivery(ctx, domain.PurchaseCollectibleUsernameRequest{
		Username: username, Buyer: buyer, CommandKey: "buy-" + suffix,
	}, purchaseUsernameDelivery); !errors.Is(err, domain.ErrStarsInsufficient) {
		t.Fatalf("err = %v, want ErrStarsInsufficient", err)
	}

	asset, err := registry.CollectibleUsername(ctx, username)
	if err != nil || asset.Status != domain.CollectibleUsernameStatusVault {
		t.Fatalf("asset after failed purchase = %+v err=%v", asset, err)
	}
	var balance int64
	if err := pool.QueryRow(ctx, `SELECT balance FROM stars_balances WHERE user_id=$1`, buyer.ID).Scan(&balance); err != nil {
		t.Fatalf("read buyer balance: %v", err)
	}
	if balance != 10 {
		t.Fatalf("buyer balance = %d, want unchanged 10", balance)
	}
}

func TestPurchaseCollectibleUsernameTONPostgres(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	suffix := randomSuffix(t)
	buyer := collectibleTestUser(t, pool, 9_300_000+int64(len(suffix)), "tonbuyer"+suffix)
	if _, err := pool.Exec(ctx, `INSERT INTO ton_balances(user_id,balance_nanoton,granted) VALUES($1,2000000000,true)`, buyer.ID); err != nil {
		t.Fatalf("seed buyer ton balance: %v", err)
	}

	registry := NewCollectibleUsernameStore(pool, WithCollectibleUsernameTONStartingGrant(0))
	username := "tonbuy" + suffix
	req := mintRequest(username, domain.Peer{}, "mint-"+suffix)
	req.Currency = domain.CollectibleCurrencyTON
	req.Amount = 1500000000
	if _, created, err := registry.MintCollectibleUsername(ctx, req); err != nil || !created {
		t.Fatalf("seed vault asset created:%v err:%v", created, err)
	}

	bought, err := registry.PurchaseCollectibleUsernameWithDelivery(ctx, domain.PurchaseCollectibleUsernameRequest{
		Username: username, Buyer: buyer, CommandKey: "buy-" + suffix,
	}, purchaseUsernameDelivery)
	if err != nil || bought.Owner != buyer {
		t.Fatalf("purchase = %+v err=%v", bought, err)
	}
	var balance int64
	if err := pool.QueryRow(ctx, `SELECT balance_nanoton FROM ton_balances WHERE user_id=$1`, buyer.ID).Scan(&balance); err != nil {
		t.Fatalf("read buyer ton balance: %v", err)
	}
	if balance != 500000000 {
		t.Fatalf("buyer ton balance = %d, want 500000000 (2e9-1.5e9)", balance)
	}
}

func TestPurchaseCollectibleUsernameNotVaultPostgres(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	suffix := randomSuffix(t)
	owner := collectibleTestUser(t, pool, 9_400_000+int64(len(suffix)), "owner"+suffix)
	buyer := collectibleTestUser(t, pool, 9_400_100+int64(len(suffix)), "wannabuy"+suffix)
	if _, err := pool.Exec(ctx, `INSERT INTO stars_balances(user_id,balance,granted) VALUES($1,999999,true)`, buyer.ID); err != nil {
		t.Fatalf("seed buyer stars balance: %v", err)
	}

	registry := NewCollectibleUsernameStore(pool)
	username := "notforsale" + suffix
	if _, created, err := registry.MintCollectibleUsername(ctx, mintRequest(username, owner, "mint-"+suffix)); err != nil || !created {
		t.Fatalf("seed owned asset created:%v err:%v", created, err)
	}

	if _, err := registry.PurchaseCollectibleUsernameWithDelivery(ctx, domain.PurchaseCollectibleUsernameRequest{
		Username: username, Buyer: buyer, CommandKey: "buy-" + suffix,
	}, purchaseUsernameDelivery); !errors.Is(err, domain.ErrCollectibleUsernameNotForSale) {
		t.Fatalf("err = %v, want ErrCollectibleUsernameNotForSale", err)
	}
}
