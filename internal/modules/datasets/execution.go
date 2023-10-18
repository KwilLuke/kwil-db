package datasets

import (
	"context"
	"fmt"
	"math/big"

	"github.com/kwilteam/kwil-db/core/types/transactions"
	"github.com/kwilteam/kwil-db/internal/accounts"
	"github.com/kwilteam/kwil-db/internal/engine"
	engineTypes "github.com/kwilteam/kwil-db/internal/engine/types"
)

/*
	This files contains logic for executing state changes against the database.
*/

// ExecutionResponse is the response from any interaction that modifies state.
type ExecutionResponse struct {
	// Fee is the amount of tokens spent on the execution
	Fee     *big.Int
	GasUsed int64
}

// Deploy deploys a database.
func (u *DatasetModule) Deploy(ctx context.Context, schema *engineTypes.Schema, tx *transactions.Transaction) (*ExecutionResponse, error) {
	price, err := u.PriceDeploy(ctx, schema)
	if err != nil {
		if price == nil {
			price = big.NewInt(0)
		}
		return resp(price), err
	}

	err = u.compareAndSpend(ctx, price, tx)
	if err != nil {
		return resp(price), err
	}

	identifier := getUserIdentifier(tx)
	if err != nil {
		return resp(price), fmt.Errorf("failed to get user identifier: %w", err)
	}

	_, err = u.engine.CreateDataset(ctx, schema, identifier)
	if err != nil {
		return resp(price), fmt.Errorf("failed to create dataset: %w", err)
	}

	return resp(price), nil
}

// Drop drops a database.
func (u *DatasetModule) Drop(ctx context.Context, dbid string, tx *transactions.Transaction) (*ExecutionResponse, error) {
	price, err := u.PriceDrop(ctx, dbid)
	if err != nil {
		if price == nil {
			price = big.NewInt(0)
		}
		return resp(price), err
	}

	err = u.compareAndSpend(ctx, price, tx)
	if err != nil {
		return resp(price), err
	}

	identifier := getUserIdentifier(tx)
	if err != nil {
		return resp(price), fmt.Errorf("failed to get user identifier: %w", err)
	}

	err = u.engine.DropDataset(ctx, dbid, identifier)
	if err != nil {
		return resp(price), fmt.Errorf("failed to drop dataset: %w", err)
	}

	return resp(price), nil
}

// Execute executes an action against a database.
func (u *DatasetModule) Execute(ctx context.Context, dbid string, action string, args [][]any, tx *transactions.Transaction) (*ExecutionResponse, error) {
	price, err := u.PriceExecute(ctx, dbid, action, args)
	if err != nil {
		if price == nil {
			price = big.NewInt(0)
		}
		return resp(price), err
	}

	err = u.compareAndSpend(ctx, price, tx)
	if err != nil {
		return resp(price), err
	}

	identifier := getUserIdentifier(tx)
	if err != nil {
		return resp(price), fmt.Errorf("failed to get user identifier: %w", err)
	}

	_, err = u.engine.Execute(ctx, dbid, action, args,
		engine.WithCaller(identifier),
	)
	if err != nil {
		return resp(price), fmt.Errorf("failed to execute action '%s' on database '%s': %w", action, dbid, err)
	}

	return resp(price), nil
}

// compareAndSpend compares the calculated price to the transaction's fee, and spends the price if the fee is sufficient.
func (u *DatasetModule) compareAndSpend(ctx context.Context, price *big.Int, tx *transactions.Transaction) error {
	if tx.Body.Fee.Cmp(price) < 0 {
		return fmt.Errorf(`%w: fee %s is less than price %s`, ErrInsufficientFee, tx.Body.Fee.String(), price.String())
	}

	return u.accountStore.Spend(ctx, &accounts.Spend{
		AccountPubKey: tx.Sender,
		Amount:        price,
		Nonce:         int64(tx.Body.Nonce),
	})
}

func resp(fee *big.Int) *ExecutionResponse {
	return &ExecutionResponse{
		Fee:     fee,
		GasUsed: 0,
	}
}

// getUserIdentifier gets the user identifier from a transaction.
func getUserIdentifier(tx *transactions.Transaction) *engineTypes.User {
	return &engineTypes.User{
		PublicKey: tx.Sender,
		AuthType:  tx.Signature.Type,
	}
}