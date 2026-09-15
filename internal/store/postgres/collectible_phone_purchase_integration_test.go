package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"telesrv/internal/domain"
)

func mintVaultPhoneRequest(phone, commandKey string) domain.MintCollectiblePhoneRequest {
	return domain.MintCollectiblePhoneRequest{
		Phone: phone, Tier: domain.CollectiblePhoneTierStandard,
		PurchaseDate: time.Now().UTC(), Currency: domain.CollectibleCurrencyUSD, Amount: 500,
		CryptoCurrency: domain.CollectibleCryptoCurrencyTON, CryptoAmount: 3_000_000_000,
		Actor: "ops", Reason: "integration test", CommandKey: commandKey,
	}
}

func TestPurchaseCollectiblePhoneTONPostgres(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	suffix := randomSuffix(t)
	users := NewUserStore(pool)
	buyer := createTestUser(t, ctx, users, "+1888"+suffix+"01", "PhoneBuyer", "")
	if _, err := pool.Exec(ctx, `INSERT INTO ton_balances(user_id,balance_nanoton,granted) VALUES($1,5000000000,true)`, buyer.ID); err != nil {
		t.Fatalf("seed buyer ton balance: %v", err)
	}

	numericSuffix, err := strconv.ParseUint(suffix, 16, 32)
	if err != nil {
		t.Fatalf("parse random phone suffix: %v", err)
	}
	phone := fmt.Sprintf("888%010d", numericSuffix)
	phones := NewCollectiblePhoneStore(pool)
	seed, created, err := phones.MintCollectiblePhoneWithDelivery(ctx, mintVaultPhoneRequest(phone, "mint-"+suffix), pgCollectiblePhoneEffects([]byte("mint")))
	if err != nil || !created || seed.Status != domain.CollectibleUsernameStatusVault {
		t.Fatalf("seed vault phone = %+v created:%v err:%v", seed, created, err)
	}

	bought, changed, err := phones.PurchaseCollectiblePhoneWithDelivery(ctx, domain.PurchaseCollectiblePhoneRequest{
		Phone: phone, BuyerUserID: buyer.ID, Actor: "buyer", Reason: "public purchase", CommandKey: "buy-" + suffix,
	}, pgCollectiblePhoneEffects([]byte("purchase")))
	if err != nil || !changed || bought.OwnerUserID != buyer.ID || bought.OriginalOwnerUserID != buyer.ID {
		t.Fatalf("purchase = %+v changed:%v err:%v", bought, changed, err)
	}
	if bought.PurchaseDate.Before(seed.PurchaseDate) {
		t.Fatalf("purchase date did not move forward: seed=%v bought=%v", seed.PurchaseDate, bought.PurchaseDate)
	}

	var balance int64
	if err := pool.QueryRow(ctx, `SELECT balance_nanoton FROM ton_balances WHERE user_id=$1`, buyer.ID).Scan(&balance); err != nil {
		t.Fatalf("read buyer ton balance: %v", err)
	}
	if balance != 2_000_000_000 {
		t.Fatalf("buyer ton balance = %d, want 2000000000 (5e9-3e9)", balance)
	}
	var reason string
	var amount int64
	if err := pool.QueryRow(ctx, `SELECT reason, amount_nanoton FROM ton_transactions WHERE user_id=$1 ORDER BY id DESC LIMIT 1`, buyer.ID).
		Scan(&reason, &amount); err != nil {
		t.Fatalf("read ton transaction: %v", err)
	}
	if reason != string(domain.StarsReasonCollectible) || amount != -3_000_000_000 {
		t.Fatalf("ton transaction reason=%q amount=%d", reason, amount)
	}

	// Replaying the same command key must not charge the buyer twice.
	replay, changed, err := phones.PurchaseCollectiblePhoneWithDelivery(ctx, domain.PurchaseCollectiblePhoneRequest{
		Phone: phone, BuyerUserID: buyer.ID, CommandKey: "buy-" + suffix,
	}, pgCollectiblePhoneEffects([]byte("purchase")))
	if err != nil || changed || replay.ID != bought.ID {
		t.Fatalf("replay purchase = %+v changed:%v err=%v", replay, changed, err)
	}
	if err := pool.QueryRow(ctx, `SELECT balance_nanoton FROM ton_balances WHERE user_id=$1`, buyer.ID).Scan(&balance); err != nil {
		t.Fatalf("read buyer ton balance after replay: %v", err)
	}
	if balance != 2_000_000_000 {
		t.Fatalf("replay double-charged: balance = %d, want 2000000000", balance)
	}
}

func TestPurchaseCollectiblePhoneInsufficientTONPostgres(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	suffix := randomSuffix(t)
	users := NewUserStore(pool)
	buyer := createTestUser(t, ctx, users, "+1888"+suffix+"02", "PoorPhoneBuyer", "")
	if _, err := pool.Exec(ctx, `INSERT INTO ton_balances(user_id,balance_nanoton,granted) VALUES($1,10,true)`, buyer.ID); err != nil {
		t.Fatalf("seed buyer ton balance: %v", err)
	}
	numericSuffix, err := strconv.ParseUint(suffix, 16, 32)
	if err != nil {
		t.Fatalf("parse random phone suffix: %v", err)
	}
	phone := fmt.Sprintf("888%010d", numericSuffix+1)
	phones := NewCollectiblePhoneStore(pool)
	if _, created, err := phones.MintCollectiblePhoneWithDelivery(ctx, mintVaultPhoneRequest(phone, "mint-"+suffix), pgCollectiblePhoneEffects([]byte("mint"))); err != nil || !created {
		t.Fatalf("seed vault phone created:%v err:%v", created, err)
	}

	if _, _, err := phones.PurchaseCollectiblePhoneWithDelivery(ctx, domain.PurchaseCollectiblePhoneRequest{
		Phone: phone, BuyerUserID: buyer.ID, CommandKey: "buy-" + suffix,
	}, pgCollectiblePhoneEffects([]byte("purchase"))); !errors.Is(err, domain.ErrStarsInsufficient) {
		t.Fatalf("err = %v, want ErrStarsInsufficient", err)
	}
	asset, err := phones.CollectiblePhone(ctx, phone)
	if err != nil || asset.Status != domain.CollectibleUsernameStatusVault {
		t.Fatalf("asset after failed purchase = %+v err=%v", asset, err)
	}
}

func TestPurchaseCollectiblePhoneNotVaultPostgres(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	suffix := randomSuffix(t)
	users := NewUserStore(pool)
	owner := createTestUser(t, ctx, users, "+1888"+suffix+"03", "PhoneOwner", "")
	buyer := createTestUser(t, ctx, users, "+1888"+suffix+"04", "WannaBuyPhone", "")
	if _, err := pool.Exec(ctx, `INSERT INTO ton_balances(user_id,balance_nanoton,granted) VALUES($1,9999999999,true)`, buyer.ID); err != nil {
		t.Fatalf("seed buyer ton balance: %v", err)
	}
	numericSuffix, err := strconv.ParseUint(suffix, 16, 32)
	if err != nil {
		t.Fatalf("parse random phone suffix: %v", err)
	}
	phone := fmt.Sprintf("888%010d", numericSuffix+2)
	phones := NewCollectiblePhoneStore(pool)
	req := mintVaultPhoneRequest(phone, "mint-"+suffix)
	req.OwnerUserID = owner.ID
	if _, created, err := phones.MintCollectiblePhoneWithDelivery(ctx, req, pgCollectiblePhoneEffects([]byte("mint"))); err != nil || !created {
		t.Fatalf("seed owned phone created:%v err:%v", created, err)
	}

	if _, _, err := phones.PurchaseCollectiblePhoneWithDelivery(ctx, domain.PurchaseCollectiblePhoneRequest{
		Phone: phone, BuyerUserID: buyer.ID, CommandKey: "buy-" + suffix,
	}, pgCollectiblePhoneEffects([]byte("purchase"))); !errors.Is(err, domain.ErrCollectiblePhoneNotForSale) {
		t.Fatalf("err = %v, want ErrCollectiblePhoneNotForSale", err)
	}
}
