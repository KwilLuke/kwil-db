// Package client contains the client for interacting with the Kwil public API.
// It's supposed to be used as go-sdk for Kwil, currently used by the Kwil CLI.

package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/kwilteam/kwil-db/core/crypto"
	"github.com/kwilteam/kwil-db/core/crypto/auth"
	"github.com/kwilteam/kwil-db/core/log"
	grpc "github.com/kwilteam/kwil-db/core/rpc/client/user"
	"github.com/kwilteam/kwil-db/core/types"
	"github.com/kwilteam/kwil-db/core/types/transactions"
	"github.com/kwilteam/kwil-db/core/utils"

	"github.com/cstockton/go-conv"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	grpcCodes "google.golang.org/grpc/codes"
	grpcStatus "google.golang.org/grpc/status"
)

// TransportClient abstracts the communication with a kwil-db node, either via
// gRPC or HTTP.
type TransportClient interface {
	Close() error
	ChainInfo(ctx context.Context) (*types.ChainInfo, error)
	Call(ctx context.Context, req *transactions.CallMessage) ([]map[string]any, error)
	TxQuery(ctx context.Context, txHash []byte) (*transactions.TcTxQueryResponse, error)
	GetTarget() string
	GetSchema(ctx context.Context, dbid string) (*transactions.Schema, error)
	Query(ctx context.Context, dbid string, query string) ([]map[string]any, error)
	ListDatabases(ctx context.Context, ownerPubKey []byte) ([]string, error)
	GetAccount(ctx context.Context, pubKey []byte, status types.AccountStatus) (*types.Account, error)
	Broadcast(ctx context.Context, tx *transactions.Transaction) ([]byte, error)
	Ping(ctx context.Context) (string, error)
	EstimateCost(ctx context.Context, tx *transactions.Transaction) (*big.Int, error)
	ValidatorJoinStatus(ctx context.Context, pubKey []byte) (*types.JoinRequest, error)
	CurrentValidators(ctx context.Context) ([]*types.Validator, error)
}

var (
	ErrNotFound = errors.New("not found")
)

// Client wraps the methods to interact with the Kwil public API.
// All the transport level details are encapsulated in the transportClient.
type Client struct {
	// transportClient is more useful for testing rn, I'd like to add http
	// client as well to test HTTP api. This also enables test the cli by mocking.
	transportClient TransportClient
	Signer          auth.Signer
	logger          log.Logger
	// chainID is used when creating transactions as replay protection since the
	// signatures will only be valid on this network.
	chainID string

	tlsCertFile string // the tls cert file path
}

// Dial creates a kwil client connection to the given target. It will by default
// use grpc, but it can be overridden by passing WithTransportClient.
func Dial(ctx context.Context, target string, opts ...Option) (c *Client, err error) {
	c = &Client{
		logger: log.NewNoOp(), // by default, we do not want to force client to log anything
	}

	for _, opt := range opts {
		opt(c)
	}

	if c.transportClient == nil {
		transportOptions := []grpc.Option{grpc.WithTlsCert(c.tlsCertFile)}
		transport, err := grpc.New(ctx, target, transportOptions...)
		if err != nil {
			return nil, err
		}
		c.transportClient = transport
	}

	zapFields := []zapcore.Field{
		zap.String("host", c.transportClient.GetTarget()),
	}

	c.logger = *c.logger.Named("client").With(zapFields...)

	chainInfo, err := c.ChainInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}
	chainID := chainInfo.ChainID
	if c.chainID == "" {
		// TODO: make this an error instead, or allowed given an Option and/or if TLS is used
		c.logger.Warn("chain ID not set, trusting chain ID from remote host!", zap.String("chainID", chainID))
		c.chainID = chainID
	} else if c.chainID != chainID {
		c.Close()
		return nil, fmt.Errorf("remote host chain ID %q != client configured %q", chainID, c.chainID)
	}

	return c, nil
}

func (c *Client) Close() error {
	return c.transportClient.Close()
}

// ChainInfo get the current blockchain information like chain ID and best block
// height/hash.
func (c *Client) ChainInfo(ctx context.Context) (*types.ChainInfo, error) {
	return c.transportClient.ChainInfo(ctx)
}

// GetSchema gets a schema by dbid.
func (c *Client) GetSchema(ctx context.Context, dbid string) (*transactions.Schema, error) {
	ds, err := c.transportClient.GetSchema(ctx, dbid)
	if err != nil {
		return nil, err
	}

	return ds, nil
}

// DeployDatabase deploys a schema
func (c *Client) DeployDatabase(ctx context.Context, payload *transactions.Schema, opts ...TxOpt) (transactions.TxHash, error) {
	tx, err := c.newTx(ctx, payload, opts...)
	if err != nil {
		return nil, err
	}

	c.logger.Debug("deploying database",
		zap.String("signature_type", tx.Signature.Type),
		zap.String("signature", base64.StdEncoding.EncodeToString(tx.Signature.Signature)))

	return c.transportClient.Broadcast(ctx, tx)
}

// DropDatabase drops a database
func (c *Client) DropDatabase(ctx context.Context, name string, opts ...TxOpt) (transactions.TxHash, error) {
	identifier := &transactions.DropSchema{
		DBID: utils.GenerateDBID(name, c.Signer.PublicKey()),
	}

	tx, err := c.newTx(ctx, identifier, opts...)
	if err != nil {
		return nil, err
	}

	c.logger.Debug("deploying database",
		zap.String("signature_type", tx.Signature.Type),
		zap.String("signature", base64.StdEncoding.EncodeToString(tx.Signature.Signature)))

	res, err := c.transportClient.Broadcast(ctx, tx)
	if err != nil {
		return nil, err
	}

	return res, nil
}

// ExecuteAction executes an action.
// It returns the receipt, as well as outputs which is the decoded body of the receipt.
// It can take any number of inputs, and if multiple tuples of inputs are passed, it will execute them transactionally.
func (c *Client) ExecuteAction(ctx context.Context, dbid string, action string, tuples [][]any, opts ...TxOpt) (transactions.TxHash, error) {
	stringTuples, err := convertTuples(tuples)
	if err != nil {
		return nil, err
	}

	executionBody := &transactions.ActionExecution{
		Action:    action,
		DBID:      dbid,
		Arguments: stringTuples,
	}

	tx, err := c.newTx(ctx, executionBody, opts...)
	if err != nil {
		return nil, err
	}

	c.logger.Debug("execute action",
		zap.String("signature_type", tx.Signature.Type),
		zap.String("signature", base64.StdEncoding.EncodeToString(tx.Signature.Signature)))

	return c.transportClient.Broadcast(ctx, tx)
}

// CallAction call an action, if auxiliary `mustsign` is set, need to sign the action payload. It returns the records.
func (c *Client) CallAction(ctx context.Context, dbid string, action string, inputs []any, opts ...CallOpt) (*Records, error) {
	callOpts := &callOptions{}

	for _, opt := range opts {
		opt(callOpts)
	}

	stringInputs, err := convertTuple(inputs)
	if err != nil {
		return nil, err
	}

	payload := &transactions.ActionCall{
		DBID:      dbid,
		Action:    action,
		Arguments: stringInputs,
	}

	shouldSign, err := shouldAuthenticate(c.Signer, callOpts.forceAuthenticated)
	if err != nil {
		return nil, err
	}

	msg, err := transactions.CreateCallMessage(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to create signed message: %w", err)
	}

	if shouldSign {
		err = msg.Sign(c.Signer)

		if err != nil {
			return nil, fmt.Errorf("failed to create signed message: %w", err)
		}
	}

	res, err := c.transportClient.Call(ctx, msg)
	if err != nil {
		return nil, err
	}

	return NewRecordsFromMaps(res), nil
}

// shouldAuthenticate decides whether the client should authenticate or not
// if enforced is not nil, it will be used instead of the default value
// otherwise, if the private key is not nil, it will authenticate
func shouldAuthenticate(signer auth.Signer, enforced *bool) (bool, error) {
	if enforced != nil {
		if !*enforced {
			return false, nil
		}

		if signer == nil {
			return false, fmt.Errorf("private key is nil, but authentication is enforced")
		}

		return true, nil
	}

	return signer != nil, nil
}

func DecodeOutputs(bts []byte) ([]map[string]any, error) {
	if len(bts) == 0 {
		return []map[string]any{}, nil
	}

	var outputs []map[string]any
	err := json.Unmarshal(bts, &outputs)
	if err != nil {
		return nil, err
	}

	return outputs, nil
}

// Query executes a query
func (c *Client) Query(ctx context.Context, dbid string, query string) (*Records, error) {
	res, err := c.transportClient.Query(ctx, dbid, query)
	if err != nil {
		return nil, err
	}

	return NewRecordsFromMaps(res), nil
}

func (c *Client) ListDatabases(ctx context.Context, ownerPubKey []byte) ([]string, error) {
	return c.transportClient.ListDatabases(ctx, ownerPubKey)
}

func (c *Client) Ping(ctx context.Context) (string, error) {
	return c.transportClient.Ping(ctx)
}

func (c *Client) GetAccount(ctx context.Context, pubKey []byte, status types.AccountStatus) (*types.Account, error) {
	return c.transportClient.GetAccount(ctx, pubKey, status)
}

func (c *Client) ValidatorJoinStatus(ctx context.Context, pubKey []byte) (*types.JoinRequest, error) {
	res, err := c.transportClient.ValidatorJoinStatus(ctx, pubKey)
	if err != nil {
		if stat, ok := grpcStatus.FromError(err); ok {
			if stat.Code() == grpcCodes.NotFound {
				return nil, ErrNotFound
			}
		}
		return nil, err
	}
	return res, nil
}

func (c *Client) CurrentValidators(ctx context.Context) ([]*types.Validator, error) {
	return c.transportClient.CurrentValidators(ctx)
}

func (c *Client) ApproveValidator(ctx context.Context, joiner []byte, opts ...TxOpt) ([]byte, error) {
	_, err := crypto.Ed25519PublicKeyFromBytes(joiner)
	if err != nil {
		return nil, fmt.Errorf("invalid candidate validator public key: %w", err)
	}
	payload := &transactions.ValidatorApprove{
		Candidate: joiner,
	}
	tx, err := c.newTx(ctx, payload, opts...)
	if err != nil {
		return nil, err
	}

	hash, err := c.transportClient.Broadcast(ctx, tx)
	if err != nil {
		return nil, err
	}
	return hash, nil
}

func (c *Client) ValidatorJoin(ctx context.Context) ([]byte, error) {
	const power = 1
	return c.validatorUpdate(ctx, power)
}

func (c *Client) ValidatorLeave(ctx context.Context) ([]byte, error) {
	return c.validatorUpdate(ctx, 0)
}

func (c *Client) validatorUpdate(ctx context.Context, power int64, opts ...TxOpt) ([]byte, error) {
	pubKey := c.Signer.PublicKey()

	var payload transactions.Payload
	if power <= 0 {
		payload = &transactions.ValidatorLeave{
			Validator: pubKey,
		}
	} else {
		payload = &transactions.ValidatorJoin{
			Candidate: pubKey,
			Power:     uint64(power),
		}
	}

	tx, err := c.newTx(ctx, payload, opts...)
	if err != nil {
		return nil, err
	}

	hash, err := c.transportClient.Broadcast(ctx, tx)
	if err != nil {
		return nil, err
	}
	return hash, nil
}

// convertTuples converts user passed tuples to strings.
// this is necessary for RLP encoding
func convertTuples(tuples [][]any) ([][]string, error) {
	ins := [][]string{}
	for _, tuple := range tuples {
		stringTuple, err := convertTuple(tuple)
		if err != nil {
			return nil, err
		}
		ins = append(ins, stringTuple)
	}

	return ins, nil
}

// convertTuple converts user passed tuple to strings.
func convertTuple(tuple []any) ([]string, error) {
	stringTuple := []string{}
	for _, val := range tuple {

		stringVal, err := conv.String(val)
		if err != nil {
			return nil, err
		}

		stringTuple = append(stringTuple, stringVal)
	}

	return stringTuple, nil
}

// TxQuery get transaction by hash
func (c *Client) TxQuery(ctx context.Context, txHash []byte) (*transactions.TcTxQueryResponse, error) {
	res, err := c.transportClient.TxQuery(ctx, txHash)
	if err != nil {
		return nil, err
	}

	return res, nil
}

// WaitTx repeatedly queries at a given interval for the status of a transaction
// until it is confirmed (is included in a block).
func (c *Client) WaitTx(ctx context.Context, txHash []byte, interval time.Duration) (*transactions.TcTxQueryResponse, error) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		resp, err := c.TxQuery(ctx, txHash)
		notFound := grpcStatus.Code(err) == grpcCodes.NotFound
		if !notFound {
			if err != nil {
				return nil, err
			}
			if resp.Height > 0 {
				return resp, nil
			}
		} else {
			// NOTE: this log may be removed once we've resolved the issue of
			// transactions not being found immediately after broadcast.
			c.logger.Debug("tx not found")
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}