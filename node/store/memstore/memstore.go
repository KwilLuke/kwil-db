// Package memstore provides a memory-backed block store, which is only suitable
// for testing where a disk-based store or third party dependencies are not desired.
package memstore

import (
	"fmt"
	"sync"

	ktypes "github.com/kwilteam/kwil-db/core/types"
	"github.com/kwilteam/kwil-db/node/types"
)

type blockHashes struct {
	hash    types.Hash
	appHash types.Hash
}

type MemBS struct {
	mtx       sync.RWMutex
	idx       map[types.Hash]int64
	hashes    map[int64]blockHashes
	blocks    map[types.Hash]*ktypes.Block
	txResults map[types.Hash][]ktypes.TxResult
	txIds     map[types.Hash]types.Hash // tx hash -> block hash
	fetching  map[types.Hash]bool       // TODO: remove, app concern
}

func NewMemBS() *MemBS {
	return &MemBS{
		idx:       make(map[types.Hash]int64),
		hashes:    make(map[int64]blockHashes),
		blocks:    make(map[types.Hash]*ktypes.Block),
		txResults: make(map[types.Hash][]ktypes.TxResult),
		txIds:     make(map[types.Hash]types.Hash),
		fetching:  make(map[types.Hash]bool),
	}
}

var _ types.BlockStore = &MemBS{}

func (bs *MemBS) Get(hash types.Hash) (*ktypes.Block, types.Hash, error) {
	bs.mtx.RLock()
	defer bs.mtx.RUnlock()
	blk, have := bs.blocks[hash]
	if !have {
		return nil, types.Hash{}, types.ErrNotFound
	}
	hashes, have := bs.hashes[blk.Header.Height]
	if !have {
		return nil, types.Hash{}, types.ErrNotFound
	}
	return blk, hashes.appHash, nil
}

func (bs *MemBS) GetByHeight(height int64) (types.Hash, *ktypes.Block, types.Hash, error) {
	// time.Sleep(100 * time.Millisecond) // wtf where is there a logic race in CE?
	bs.mtx.RLock()
	defer bs.mtx.RUnlock()
	blkHash, have := bs.hashes[height]
	if !have {
		return types.Hash{}, nil, types.Hash{}, types.ErrNotFound
	}
	blk, have := bs.blocks[blkHash.hash]
	if !have {
		return types.Hash{}, nil, types.Hash{}, types.ErrNotFound
	}
	return blkHash.hash, blk, blkHash.appHash, nil
}

func (bs *MemBS) Have(blkid types.Hash) bool {
	bs.mtx.RLock()
	defer bs.mtx.RUnlock()
	_, have := bs.idx[blkid]
	return have
}

func (bs *MemBS) Store(block *ktypes.Block, appHash types.Hash) error {
	bs.mtx.Lock()
	defer bs.mtx.Unlock()
	blkHash := block.Hash()
	bs.blocks[blkHash] = block
	bs.idx[blkHash] = block.Header.Height
	bs.hashes[block.Header.Height] = blockHashes{
		hash:    blkHash,
		appHash: appHash,
	}
	for _, tx := range block.Txns {
		txHash := types.HashBytes(tx)
		bs.txIds[txHash] = blkHash
	}
	return nil
}

func (bs *MemBS) StoreResults(hash types.Hash, results []ktypes.TxResult) error {
	bs.mtx.Lock()
	defer bs.mtx.Unlock()
	bs.txResults[hash] = results
	return nil
}

func (bs *MemBS) Results(hash types.Hash) ([]ktypes.TxResult, error) {
	bs.mtx.RLock()
	defer bs.mtx.RUnlock()
	res, have := bs.txResults[hash]
	if !have {
		return nil, types.ErrNotFound
	}
	return res, nil
}

func (bs *MemBS) Result(hash types.Hash, idx uint32) (*ktypes.TxResult, error) {
	bs.mtx.RLock()
	defer bs.mtx.RUnlock()
	res, have := bs.txResults[hash]
	if !have {
		return nil, types.ErrNotFound
	}
	if int(idx) >= len(res) {
		return nil, fmt.Errorf("%w: invalid block index", types.ErrNotFound)
	}
	r := res[idx]
	return &r, nil
}

func (bs *MemBS) Best() (int64, types.Hash, types.Hash) {
	bs.mtx.RLock()
	defer bs.mtx.RUnlock()
	var bestHeight int64
	var bestHash, bestAppHash types.Hash
	for height, hashes := range bs.hashes {
		if height >= bestHeight {
			bestHeight = height
			bestHash = hashes.hash
			bestAppHash = hashes.appHash
		}
	}
	return bestHeight, bestHash, bestAppHash
}

func (bs *MemBS) PreFetch(blkid types.Hash) (bool, func()) {
	bs.mtx.Lock()
	defer bs.mtx.Unlock()
	if _, have := bs.idx[blkid]; have {
		return false, func() {} // don't need it
	}

	if fetching := bs.fetching[blkid]; fetching {
		return false, func() {} // already getting it
	}
	bs.fetching[blkid] = true

	return true, func() {
		bs.mtx.Lock()
		delete(bs.fetching, blkid)
		bs.mtx.Unlock()
	} // go get it
}

func (bs *MemBS) Close() error { return nil }

func (bs *MemBS) GetTx(txHash types.Hash) (tx *ktypes.Transaction, height int64, hash types.Hash, idx uint32, err error) {
	bs.mtx.RLock()
	defer bs.mtx.RUnlock()
	// check the tx index, pull the block and then search for the tx with the expected hash
	blkHash, have := bs.txIds[txHash]
	if !have {
		return nil, 0, types.Hash{}, 0, types.ErrNotFound
	}
	blk, have := bs.blocks[blkHash]
	if !have {
		return nil, 0, types.Hash{}, 0, types.ErrNotFound
	}
	for idx, rawTx := range blk.Txns {
		if types.HashBytes(rawTx) == txHash {
			tx := new(ktypes.Transaction)
			if err = tx.UnmarshalBinary(rawTx); err != nil {
				return nil, 0, types.Hash{}, 0, err
			}
			return tx, blk.Header.Height, blk.Hash(), uint32(idx), nil
		}
	}
	return nil, 0, types.Hash{}, 0, types.ErrNotFound
}

func (bs *MemBS) HaveTx(txHash types.Hash) bool {
	bs.mtx.RLock()
	defer bs.mtx.RUnlock()
	_, have := bs.txIds[txHash]
	return have
}
