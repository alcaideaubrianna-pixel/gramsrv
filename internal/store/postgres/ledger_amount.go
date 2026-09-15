package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"telesrv/internal/domain"
)

// ensureTonGrantAmountTx lazily grants userID's starting TON balance the first
// time any TON-denominated ledger operation touches them, mirroring the Stars
// lazy-grant pattern. Shared by every store that debits/credits the TON ledger
// (Star Gifts, collectible usernames/phones): they all pass their own
// configured starting-grant amount so the grant stays consistent per node
// without requiring a single shared struct.
func ensureTonGrantAmountTx(ctx context.Context, tx pgx.Tx, userID int64, date int, tonStartingGrant int64) (int64, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO ton_balances(user_id,balance_nanoton,granted) VALUES($1,0,false)
ON CONFLICT(user_id) DO NOTHING`, userID); err != nil {
		return 0, err
	}
	var balance int64
	var granted bool
	if err := tx.QueryRow(ctx, `SELECT balance_nanoton,granted FROM ton_balances WHERE user_id=$1 FOR UPDATE`, userID).
		Scan(&balance, &granted); err != nil {
		return 0, err
	}
	if granted {
		return balance, nil
	}
	if err := tx.QueryRow(ctx, `UPDATE ton_balances SET balance_nanoton=balance_nanoton+$2,granted=true,updated_at=now()
WHERE user_id=$1 RETURNING balance_nanoton`, userID, tonStartingGrant).Scan(&balance); err != nil {
		return 0, err
	}
	if tonStartingGrant > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO ton_transactions(user_id,amount_nanoton,reason,date)
VALUES($1,$2,$3,$4)`, userID, tonStartingGrant, string(domain.StarsReasonGrant), date); err != nil {
			return 0, err
		}
	}
	return balance, nil
}

// debitLedgerAmountTx debits userID's Stars or TON balance, whichever amount.Currency
// selects, recording a ledger transaction with reason/peer/title. It is the
// shared payment primitive behind every "pay to acquire something" flow:
// Star Gift purchase/resale/offer/craft/auction and collectible username/phone
// purchase all fund themselves through this one function so the two ledgers
// only have one debit implementation each to get right.
func debitLedgerAmountTx(ctx context.Context, tx pgx.Tx, userID int64, amount domain.StarGiftAmount,
	reason domain.StarsTransactionReason, peer domain.Peer, date int, title string, tonStartingGrant int64) (domain.StarsBalance, error) {
	if amount.Amount == 0 {
		var balance domain.StarsBalance
		balance.UserID = userID
		err := tx.QueryRow(ctx, `SELECT balance,granted FROM stars_balances WHERE user_id=$1`, userID).Scan(&balance.Balance, &balance.Granted)
		if errors.Is(err, pgx.ErrNoRows) {
			return balance, nil
		}
		return balance, err
	}
	if amount.Currency == domain.StarGiftCurrencyTON {
		if _, err := ensureTonGrantAmountTx(ctx, tx, userID, date, tonStartingGrant); err != nil {
			return domain.StarsBalance{}, err
		}
		var balance int64
		if err := tx.QueryRow(ctx, `UPDATE ton_balances SET balance_nanoton=balance_nanoton-$2,updated_at=now()
		 WHERE user_id=$1 AND balance_nanoton>=$2 RETURNING balance_nanoton`, userID, amount.Amount).Scan(&balance); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.StarsBalance{}, domain.ErrStarsInsufficient
			}
			return domain.StarsBalance{}, err
		}
		_, err := tx.Exec(ctx, `INSERT INTO ton_transactions(user_id,amount_nanoton,reason,peer_type,peer_id,date)
		 VALUES($1,$2,$3,$4,$5,$6)`, userID, -amount.Amount, string(reason), nullableStarGiftPeerType(peer), nullableStarGiftPeerID(peer), date)
		return domain.StarsBalance{UserID: userID, Balance: balance}, err
	}
	result := domain.StarsBalance{UserID: userID}
	var current int64
	if err := tx.QueryRow(ctx, `SELECT balance,granted FROM stars_balances WHERE user_id=$1 FOR UPDATE`, userID).Scan(&current, &result.Granted); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.StarsBalance{}, domain.ErrStarsInsufficient
		}
		return domain.StarsBalance{}, err
	}
	if current < amount.Amount {
		return domain.StarsBalance{}, domain.ErrStarsInsufficient
	}
	if err := tx.QueryRow(ctx, `UPDATE stars_balances SET balance=balance-$2,updated_at=now() WHERE user_id=$1 RETURNING balance`, userID, amount.Amount).Scan(&result.Balance); err != nil {
		return domain.StarsBalance{}, err
	}
	if err := insertStarsTxn(ctx, tx, userID, -amount.Amount, reason, peer, date, title, ""); err != nil {
		return domain.StarsBalance{}, err
	}
	return result, nil
}

// creditLedgerAmountTx is debitLedgerAmountTx's counterpart: it mints amount
// into userID's Stars or TON balance. Unlike the debit side it never needs the
// TON starting-grant amount, since crediting never needs to lazily initialise
// a balance row first (the upsert below creates it if missing).
func creditLedgerAmountTx(ctx context.Context, tx pgx.Tx, userID int64, amount domain.StarGiftAmount,
	reason domain.StarsTransactionReason, peer domain.Peer, date int, title string) error {
	if amount.Currency == domain.StarGiftCurrencyTON {
		if _, err := tx.Exec(ctx, `INSERT INTO ton_balances(user_id,balance_nanoton,granted) VALUES($1,$2,false)
		 ON CONFLICT(user_id) DO UPDATE SET balance_nanoton=ton_balances.balance_nanoton+EXCLUDED.balance_nanoton,updated_at=now()`, userID, amount.Amount); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO ton_transactions(user_id,amount_nanoton,reason,peer_type,peer_id,date)
		 VALUES($1,$2,$3,$4,$5,$6)`, userID, amount.Amount, string(reason), nullableStarGiftPeerType(peer), nullableStarGiftPeerID(peer), date)
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO stars_balances(user_id,balance,updated_at) VALUES($1,$2,now())
	 ON CONFLICT(user_id) DO UPDATE SET balance=stars_balances.balance+EXCLUDED.balance,updated_at=now()`, userID, amount.Amount); err != nil {
		return err
	}
	return insertStarsTxn(ctx, tx, userID, amount.Amount, reason, peer, date, title, "")
}
