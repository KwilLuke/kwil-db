package adminsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/kwilteam/kwil-db/config"
	"github.com/kwilteam/kwil-db/core/crypto/auth"
	"github.com/kwilteam/kwil-db/core/log"
	jsonrpc "github.com/kwilteam/kwil-db/core/rpc/json"
	adminjson "github.com/kwilteam/kwil-db/core/rpc/json/admin"
	userjson "github.com/kwilteam/kwil-db/core/rpc/json/user"
	ktypes "github.com/kwilteam/kwil-db/core/types"
	types "github.com/kwilteam/kwil-db/core/types/admin"
	"github.com/kwilteam/kwil-db/extensions/resolutions"
	rpcserver "github.com/kwilteam/kwil-db/node/services/jsonrpc"
	nodetypes "github.com/kwilteam/kwil-db/node/types"
	"github.com/kwilteam/kwil-db/node/types/sql"
	"github.com/kwilteam/kwil-db/node/voting"
	"github.com/kwilteam/kwil-db/version"
)

// BlockchainTransactor specifies the methods required for the admin service to
// interact with the blockchain.
type Node interface {
	Status(context.Context) (*types.Status, error)
	Peers(context.Context) ([]*types.PeerInfo, error)
	BroadcastTx(ctx context.Context, tx *ktypes.Transaction, sync uint8) (*ktypes.ResultBroadcastTx, error)
}

type P2P interface {
	// AddPeer adds a peer to the node's peer list and persists it.
	AddPeer(ctx context.Context, nodeID string) error

	// RemovePeer removes a peer from the node's peer list permanently.
	RemovePeer(ctx context.Context, nodeID string) error

	// ListPeers returns the list of peers in the node's whitelist.
	ListPeers(ctx context.Context) []string
}

type App interface {
	// AccountInfo returns the unconfirmed account info for the given identifier.
	// If unconfirmed is true, the account found in the mempool is returned.
	// Otherwise, the account found in the blockchain is returned.
	AccountInfo(ctx context.Context, db sql.DB, identifier []byte, unconfirmed bool) (balance *big.Int, nonce int64, err error)
	Price(ctx context.Context, db sql.DB, tx *ktypes.Transaction) (*big.Int, error)
}

type Validators interface {
	SetValidatorPower(ctx context.Context, tx sql.Executor, pubKey []byte, power int64) error
	GetValidatorPower(ctx context.Context, pubKey []byte) (int64, error)
	GetValidators() []*ktypes.Validator
}

type Service struct {
	log log.Logger

	blockchain Node // node is the local node that can accept transactions.
	app        App
	voting     Validators
	db         sql.DelayedReadTxMaker
	p2p        P2P

	cfg     *config.Config
	chainID string
	signer  auth.Signer // ed25519 signer derived from the node's private key
}

const (
	apiVerMajor = 0
	apiVerMinor = 2
	apiVerPatch = 0

	serviceName = "admin"
)

// API version log
//
// apiVerMinor = 2 indicates the presence of the peer whitelist, resolution, and
// health methods added in Kwil v0.9

var (
	apiSemver = fmt.Sprintf("%d.%d.%d", apiVerMajor, apiVerMinor, apiVerPatch)
)

func verHandler(context.Context, *userjson.VersionRequest) (*userjson.VersionResponse, *jsonrpc.Error) {
	return &userjson.VersionResponse{
		Service:     serviceName,
		Version:     apiSemver,
		Major:       apiVerMajor,
		Minor:       apiVerMinor,
		Patch:       apiVerPatch,
		KwilVersion: version.KwilVersion,
	}, nil
}

// The admin Service must be usable as a Svc registered with a JSON-RPC Server.
var _ rpcserver.Svc = (*Service)(nil)

func (svc *Service) Name() string {
	return serviceName
}

func (svc *Service) Health(ctx context.Context) (json.RawMessage, bool) {
	healthResp, jsonErr := svc.HealthMethod(ctx, &userjson.HealthRequest{})
	if jsonErr != nil { // unable to even perform the health check
		// This is not for a JSON-RPC client.
		svc.log.Error("health check failure", "error", jsonErr)
		resp, _ := json.Marshal(struct {
			Healthy bool `json:"healthy"`
		}{}) // omit everything else since
		return resp, false
	}

	resp, _ := json.Marshal(healthResp)

	return resp, healthResp.Healthy
}

// HealthMethod is a JSON-RPC method handler for service health.
func (svc *Service) HealthMethod(ctx context.Context, _ *userjson.HealthRequest) (*adminjson.HealthResponse, *jsonrpc.Error) {
	vals, jsonErr := svc.ListValidators(ctx, &adminjson.ListValidatorsRequest{})
	if jsonErr != nil {
		return nil, jsonErr
	}

	status, err := svc.blockchain.Status(ctx)
	if err != nil {
		svc.log.Error("chain status error", "error", err)
		return nil, jsonrpc.NewError(jsonrpc.ErrorNodeInternal, "status failure", nil)
	}

	// health criteria: presently, nothing, we're just here.
	// Being a validator may be a concern to the consumer.
	happy := true

	return &adminjson.HealthResponse{
		Healthy:       happy,
		Version:       apiSemver,
		PubKey:        status.Validator.PubKey,
		Role:          status.Validator.Role,
		NumValidators: len(vals.Validators),
	}, nil
	// slices.ContainsFunc(vals.Validators, func(v *ktypes.Validator) bool { return bytes.Equal(v.PubKey, status.Validator.PubKey) })
}

func (svc *Service) Methods() map[jsonrpc.Method]rpcserver.MethodDef {
	return map[jsonrpc.Method]rpcserver.MethodDef{
		adminjson.MethodVersion: rpcserver.MakeMethodDef(verHandler,
			"retrieve the API version of the admin service",    // method description
			"service info including semver and kwild version"), // return value description
		adminjson.MethodStatus: rpcserver.MakeMethodDef(svc.Status,
			"retrieve node status",
			"node information including name, chain id, sync, identity, etc."),
		adminjson.MethodPeers: rpcserver.MakeMethodDef(svc.Peers,
			"get the current peers of the node",
			"a list of the node's current peers"),
		adminjson.MethodConfig: rpcserver.MakeMethodDef(svc.GetConfig,
			"retrieve the current effective node config",
			"the raw bytes of the effective config TOML document"),
		adminjson.MethodValApprove: rpcserver.MakeMethodDef(svc.Approve,
			"approve a validator join request",
			"the hash of the broadcasted validator approve transaction"),
		adminjson.MethodValJoin: rpcserver.MakeMethodDef(svc.Join,
			"request the node to become a validator",
			"the hash of the broadcasted validator join transaction"),
		adminjson.MethodValJoinStatus: rpcserver.MakeMethodDef(svc.JoinStatus,
			"query for the status of a validator join request",
			"the pending join request details, if it exists"),
		adminjson.MethodValListJoins: rpcserver.MakeMethodDef(svc.ListPendingJoins,
			"list active validator join requests",
			"all pending join requests including the current approvals and the join expiry"),
		adminjson.MethodValList: rpcserver.MakeMethodDef(svc.ListValidators,
			"list the current validators",
			"the list of current validators and their power"),
		adminjson.MethodValLeave: rpcserver.MakeMethodDef(svc.Leave,
			"leave the validator set",
			"the hash of the broadcasted validator leave transaction"),
		adminjson.MethodValRemove: rpcserver.MakeMethodDef(svc.Remove,
			"vote to remote a validator",
			"the hash of the broadcasted validator remove transaction"),
		adminjson.MethodAddPeer: rpcserver.MakeMethodDef(svc.AddPeer,
			"add a peer to the network", ""),
		adminjson.MethodRemovePeer: rpcserver.MakeMethodDef(svc.RemovePeer,
			"add a peer to the network",
			""),
		adminjson.MethodListPeers: rpcserver.MakeMethodDef(svc.ListPeers,
			"list the peers from the node's whitelist",
			"the list of peers from which the node can accept connections from."),
		adminjson.MethodCreateResolution: rpcserver.MakeMethodDef(svc.CreateResolution,
			"create a resolution",
			"the hash of the broadcasted create resolution transaction",
		),
		adminjson.MethodApproveResolution: rpcserver.MakeMethodDef(svc.ApproveResolution,
			"approve a resolution",
			"the hash of the broadcasted approve resolution transaction",
		),
		// adminjson.MethodDeleteResolution: rpcserver.MakeMethodDef(svc.DeleteResolution,
		// 	"delete a resolution",
		// 	"the hash of the broadcasted delete resolution transaction",
		// ),
		adminjson.MethodResolutionStatus: rpcserver.MakeMethodDef(svc.ResolutionStatus,
			"get the status of a resolution",
			"the status of the resolution"),
		adminjson.MethodHealth: rpcserver.MakeMethodDef(svc.HealthMethod,
			"check the admin service health",
			"the health status and other relevant of the services health",
		),
	}
}

func (svc *Service) Handlers() map[jsonrpc.Method]rpcserver.MethodHandler {
	handlers := make(map[jsonrpc.Method]rpcserver.MethodHandler)
	for method, def := range svc.Methods() {
		handlers[method] = def.Handler
	}
	return handlers
}

// NewService constructs a new Service.
func NewService(db sql.DelayedReadTxMaker, blockchain Node, app App,
	vs Validators, p2p P2P, txSigner auth.Signer, cfg *config.Config,
	chainID string, logger log.Logger) *Service {
	return &Service{
		blockchain: blockchain,
		p2p:        p2p,
		app:        app,
		voting:     vs,
		signer:     txSigner,
		chainID:    chainID,
		cfg:        cfg,
		log:        logger,
		db:         db,
	}
}

func convertSyncInfo(si *types.SyncInfo) *adminjson.SyncInfo {
	return &adminjson.SyncInfo{
		AppHash:         si.AppHash,
		BestBlockHash:   si.BestBlockHash,
		BestBlockHeight: si.BestBlockHeight,
		BestBlockTime:   si.BestBlockTime.UnixMilli(), // this is why we dup this
		Syncing:         si.Syncing,
	}
}

func (svc *Service) Status(ctx context.Context, req *adminjson.StatusRequest) (*adminjson.StatusResponse, *jsonrpc.Error) {
	status, err := svc.blockchain.Status(ctx)
	if err != nil {
		return nil, jsonrpc.NewError(jsonrpc.ErrorNodeInternal, "node status unavailable", nil)
	}

	var power int64
	switch status.Validator.Role {
	case nodetypes.RoleLeader.String(), nodetypes.RoleValidator.String():
		power, _ = svc.voting.GetValidatorPower(ctx, status.Validator.PubKey)
	}

	return &adminjson.StatusResponse{
		Node: status.Node,
		Sync: convertSyncInfo(status.Sync),
		Validator: &adminjson.Validator{ // TODO: weed out the type dups
			Role:   status.Validator.Role,
			PubKey: status.Validator.PubKey,
			Power:  power,
		},
	}, nil
}

func (svc *Service) Peers(ctx context.Context, _ *adminjson.PeersRequest) (*adminjson.PeersResponse, *jsonrpc.Error) {
	peers, err := svc.blockchain.Peers(ctx)
	if err != nil {
		return nil, jsonrpc.NewError(jsonrpc.ErrorNodeInternal, "node peers unavailable", nil)
	}
	// pbPeers := make([]*types.PeerInfo, len(peers))
	// for i, p := range peers {
	// 	pbPeers[i] = &types.PeerInfo{
	// 		NodeInfo:   p.NodeInfo,
	// 		Inbound:    p.Inbound,
	// 		RemoteAddr: p.RemoteAddr,
	// 	}
	// }
	return &adminjson.PeersResponse{
		Peers: peers,
	}, nil
}

// sendTx makes a transaction and sends it to the local node.
func (svc *Service) sendTx(ctx context.Context, payload ktypes.Payload) (*userjson.BroadcastResponse, *jsonrpc.Error) {
	readTx := svc.db.BeginDelayedReadTx()
	defer readTx.Rollback(ctx)

	// Get the latest nonce for the account, if it exists.
	_, nonce, err := svc.app.AccountInfo(ctx, readTx, svc.signer.Identity(), true)
	if err != nil {
		return nil, jsonrpc.NewError(jsonrpc.ErrorAccountInternal, "account info error", nil)
	}

	tx, err := ktypes.CreateNodeTransaction(payload, svc.chainID, uint64(nonce+1))
	if err != nil {
		return nil, jsonrpc.NewError(jsonrpc.ErrorInternal, "unable to create transaction", nil)
	}

	fee, err := svc.app.Price(ctx, readTx, tx)
	if err != nil {
		return nil, jsonrpc.NewError(jsonrpc.ErrorTxInternal, "unable to price transaction", nil)
	}

	tx.Body.Fee = fee

	// Sign the transaction.
	err = tx.Sign(svc.signer)
	if err != nil {
		return nil, jsonrpc.NewError(jsonrpc.ErrorInternal, "signing transaction failed", nil)
	}

	res, err := svc.blockchain.BroadcastTx(ctx, tx, uint8(userjson.BroadcastSyncSync))
	if err != nil {
		svc.log.Error("failed to broadcast tx", "error", err)
		return nil, jsonrpc.NewError(jsonrpc.ErrorTxInternal, "failed to broadcast transaction", nil)
	}

	code, txHash := res.Code, res.Hash

	if txCode := ktypes.TxCode(code); txCode != ktypes.CodeOk {
		errData := &userjson.BroadcastError{
			TxCode:  uint32(txCode), // e.g. invalid nonce, wrong chain, etc.
			Hash:    txHash.String(),
			Message: res.Log,
		}
		data, _ := json.Marshal(errData)
		return nil, jsonrpc.NewError(jsonrpc.ErrorTxExecFailure, "broadcast error", data)
	}

	svc.log.Info("broadcast transaction", "hash", txHash.String(), "nonce", tx.Body.Nonce)
	return &userjson.BroadcastResponse{
		TxHash: txHash,
	}, nil

}

func (svc *Service) Approve(ctx context.Context, req *adminjson.ApproveRequest) (*userjson.BroadcastResponse, *jsonrpc.Error) {
	return svc.sendTx(ctx, &ktypes.ValidatorApprove{
		Candidate: req.PubKey,
	})
}

func (svc *Service) Join(ctx context.Context, req *adminjson.JoinRequest) (*userjson.BroadcastResponse, *jsonrpc.Error) {
	return svc.sendTx(ctx, &ktypes.ValidatorJoin{
		Power: 1,
	})
}

func (svc *Service) Remove(ctx context.Context, req *adminjson.RemoveRequest) (*userjson.BroadcastResponse, *jsonrpc.Error) {
	return svc.sendTx(ctx, &ktypes.ValidatorRemove{
		Validator: req.PubKey,
	})
}

func (svc *Service) JoinStatus(ctx context.Context, req *adminjson.JoinStatusRequest) (*adminjson.JoinStatusResponse, *jsonrpc.Error) {
	readTx := svc.db.BeginDelayedReadTx()
	defer readTx.Rollback(ctx)
	ids, err := voting.GetResolutionIDsByTypeAndProposer(ctx, readTx, voting.ValidatorJoinEventType, req.PubKey)
	if err != nil {
		svc.log.Error("failed to retrieve join request", "error", err)
		return nil, jsonrpc.NewError(jsonrpc.ErrorDBInternal, "failed to retrieve join request", nil)
	}
	if len(ids) == 0 {
		return nil, jsonrpc.NewError(jsonrpc.ErrorValidatorNotFound, "no active join request", nil)
	}

	resolution, err := voting.GetResolutionInfo(ctx, readTx, ids[0])
	if err != nil {
		svc.log.Error("failed to retrieve join request", "error", err)
		return nil, jsonrpc.NewError(jsonrpc.ErrorDBInternal, "failed to retrieve join request details", nil)
	}

	voters := svc.voting.GetValidators()

	pendingJoin, err := toPendingInfo(resolution, voters)
	if err != nil {
		svc.log.Error("failed to convert join request", "error", err)
		return nil, jsonrpc.NewError(jsonrpc.ErrorResultEncoding, "failed to convert join request", nil)
	}

	return &adminjson.JoinStatusResponse{
		JoinRequest: pendingJoin,
	}, nil
}

func (svc *Service) Leave(ctx context.Context, req *adminjson.LeaveRequest) (*userjson.BroadcastResponse, *jsonrpc.Error) {
	return svc.sendTx(ctx, &ktypes.ValidatorLeave{})
}

func (svc *Service) ListValidators(ctx context.Context, req *adminjson.ListValidatorsRequest) (*adminjson.ListValidatorsResponse, *jsonrpc.Error) {
	vals := svc.voting.GetValidators()

	pbValidators := make([]*adminjson.Validator, len(vals))
	for i, vi := range vals {
		pbValidators[i] = &adminjson.Validator{
			Role:   vi.Role,
			PubKey: vi.PubKey,
			Power:  vi.Power,
		}
	}

	return &adminjson.ListValidatorsResponse{
		Validators: pbValidators,
	}, nil
}

func (svc *Service) ListPendingJoins(ctx context.Context, req *adminjson.ListJoinRequestsRequest) (*adminjson.ListJoinRequestsResponse, *jsonrpc.Error) {
	readTx := svc.db.BeginDelayedReadTx()
	defer readTx.Rollback(ctx)

	activeJoins, err := voting.GetResolutionsByType(ctx, readTx, voting.ValidatorJoinEventType)
	if err != nil {
		svc.log.Error("failed to retrieve active join requests", "error", err)
		return nil, jsonrpc.NewError(jsonrpc.ErrorDBInternal, "failed to retrieve active join requests", nil)
	}

	voters := svc.voting.GetValidators()

	pbJoins := make([]*adminjson.PendingJoin, len(activeJoins))
	for i, ji := range activeJoins {
		pbJoins[i], err = toPendingInfo(ji, voters)
		if err != nil {
			svc.log.Error("failed to convert join request", "error", err)
			return nil, jsonrpc.NewError(jsonrpc.ErrorResultEncoding, "failed to convert join request", nil)
		}
	}

	return &adminjson.ListJoinRequestsResponse{
		JoinRequests: pbJoins,
	}, nil
}

// toPendingInfo gets the pending information for an active join from a resolution
func toPendingInfo(resolution *resolutions.Resolution, allVoters []*ktypes.Validator) (*adminjson.PendingJoin, error) {
	resolutionBody := &voting.UpdatePowerRequest{}
	if err := resolutionBody.UnmarshalBinary(resolution.Body); err != nil {
		return nil, fmt.Errorf("failed to unmarshal join request")
	}

	// to create the board, we will take a list of all approvers and append the voters.
	// we will then remove any duplicates the second time we see them.
	// this will result with all approvers at the start of the list, and all voters at the end.
	// finally, the approvals will be true for the length of the approvers, and false for found.length - voters.length
	board := make([][]byte, 0, len(allVoters))
	approvals := make([]bool, len(allVoters))
	for i, v := range resolution.Voters {
		board = append(board, v.PubKey)
		approvals[i] = true
	}
	for _, v := range allVoters {
		board = append(board, v.PubKey)
	}

	// we will now remove duplicates from the board.
	found := make(map[string]struct{})
	for i := 0; i < len(board); i++ {
		if _, ok := found[string(board[i])]; ok {
			board = append(board[:i], board[i+1:]...)
			i--
			continue
		}
		found[string(board[i])] = struct{}{}
	}

	return &adminjson.PendingJoin{
		Candidate: resolutionBody.PubKey,
		Power:     resolutionBody.Power,
		ExpiresAt: resolution.ExpirationHeight,
		Board:     board,
		Approved:  approvals,
	}, nil
}

func (svc *Service) GetConfig(ctx context.Context, req *adminjson.GetConfigRequest) (*adminjson.GetConfigResponse, *jsonrpc.Error) {
	bts, err := svc.cfg.ToTOML()
	if err != nil {
		return nil, jsonrpc.NewError(jsonrpc.ErrorResultEncoding, "failed to encode node config", nil)
	}

	return &adminjson.GetConfigResponse{
		Config: bts,
	}, nil
}

func (svc *Service) AddPeer(ctx context.Context, req *adminjson.PeerRequest) (*adminjson.PeerResponse, *jsonrpc.Error) {
	err := svc.p2p.AddPeer(ctx, req.PeerID)
	if err != nil {
		return nil, jsonrpc.NewError(jsonrpc.ErrorInternal, "failed to add a peer. Reason: "+err.Error(), nil)
	}
	return &adminjson.PeerResponse{}, nil
}

func (svc *Service) RemovePeer(ctx context.Context, req *adminjson.PeerRequest) (*adminjson.PeerResponse, *jsonrpc.Error) {
	err := svc.p2p.RemovePeer(ctx, req.PeerID)
	if err != nil {
		svc.log.Error("failed to remove peer", "error", err)
		return nil, jsonrpc.NewError(jsonrpc.ErrorInternal, "failed to remove peer : "+err.Error(), nil)
	}
	return &adminjson.PeerResponse{}, nil
}

func (svc *Service) ListPeers(ctx context.Context, req *adminjson.PeersRequest) (*adminjson.ListPeersResponse, *jsonrpc.Error) {
	return &adminjson.ListPeersResponse{
		Peers: svc.p2p.ListPeers(ctx),
	}, nil
}

func (svc *Service) CreateResolution(ctx context.Context, req *adminjson.CreateResolutionRequest) (*userjson.BroadcastResponse, *jsonrpc.Error) {
	res := &ktypes.CreateResolution{
		Resolution: &ktypes.VotableEvent{
			Type: req.ResolutionType,
			Body: req.Resolution,
		},
	}

	return svc.sendTx(ctx, res)
}

func (svc *Service) ApproveResolution(ctx context.Context, req *adminjson.ApproveResolutionRequest) (*userjson.BroadcastResponse, *jsonrpc.Error) {
	res := &ktypes.ApproveResolution{
		ResolutionID: req.ResolutionID,
	}

	return svc.sendTx(ctx, res)
}

/* disabled until the tx route is tested
func (svc *Service) DeleteResolution(ctx context.Context, req *adminjson.DeleteResolutionRequest) (*userjson.BroadcastResponse, *jsonrpc.Error) {
	res := &ktypes.DeleteResolution{
		ResolutionID: req.ResolutionID,
	}

	return svc.sendTx(ctx, res)
}
*/

func (svc *Service) ResolutionStatus(ctx context.Context, req *adminjson.ResolutionStatusRequest) (*adminjson.ResolutionStatusResponse, *jsonrpc.Error) {
	readTx := svc.db.BeginDelayedReadTx()
	defer readTx.Rollback(ctx)

	svc.voting.GetValidators()
	uuid := req.ResolutionID
	resolution, err := voting.GetResolutionInfo(ctx, readTx, uuid)
	if err != nil {
		return nil, jsonrpc.NewError(jsonrpc.ErrorDBInternal, "failed to retrieve resolution", nil)
	}

	// get the status of the resolution
	// expiresAt, board, approvals, err := voting.ResolutionStatus(ctx, readTx, resolution)
	// if err != nil {
	// 	return nil, jsonrpc.NewError(jsonrpc.ErrorDBInternal, "failed to retrieve resolution status", nil)
	// }

	return &adminjson.ResolutionStatusResponse{
		Status: &ktypes.PendingResolution{
			ResolutionID: req.ResolutionID,
			ExpiresAt:    resolution.ExpirationHeight,
			Board:        nil, // resolution.Voters ???
			Approved:     nil, // ???
		},
	}, nil
}
