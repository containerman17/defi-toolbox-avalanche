package lightclient

import (
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// ─── Snapshot types ─────────────────────────────────────────────────

// Snapshot is the gob-serializable representation of the latest versioned state.
// Only the most recent value per key is stored; history is transient.
type Snapshot struct {
	BlockNumber uint64
	Storage     []SnapshotStorageEntry
	Balances    []SnapshotBalanceEntry
	Nonces      []SnapshotNonceEntry
	Code        []SnapshotCodeEntry
}

// SnapshotStorageEntry is a single storage slot.
type SnapshotStorageEntry struct {
	Addr  common.Address
	Slot  common.Hash
	Value common.Hash
}

// SnapshotBalanceEntry is a single account balance stored as big-endian uint256.
type SnapshotBalanceEntry struct {
	Addr    common.Address
	Balance [32]byte // uint256 big-endian via Bytes32()
}

// SnapshotNonceEntry is a single account nonce.
type SnapshotNonceEntry struct {
	Addr  common.Address
	Nonce uint64
}

// SnapshotCodeEntry is a single account's code.
type SnapshotCodeEntry struct {
	Addr common.Address
	Code []byte
}

// ─── SaveSnapshot ───────────────────────────────────────────────────

// SaveSnapshot serializes the latest value of every key in the versioned state
// to the given path. Writes atomically: tmp file -> fsync -> rename.
func SaveSnapshot(state *VersionedState, path string) error {
	snap := Snapshot{
		BlockNumber: state.LatestBlock(),
	}

	state.ForEachStorage(func(addr common.Address, slot common.Hash, value common.Hash) {
		snap.Storage = append(snap.Storage, SnapshotStorageEntry{
			Addr:  addr,
			Slot:  slot,
			Value: value,
		})
	})

	state.ForEachBalance(func(addr common.Address, balance *uint256.Int) {
		snap.Balances = append(snap.Balances, SnapshotBalanceEntry{
			Addr:    addr,
			Balance: balance.Bytes32(),
		})
	})

	state.ForEachNonce(func(addr common.Address, nonce uint64) {
		snap.Nonces = append(snap.Nonces, SnapshotNonceEntry{
			Addr:  addr,
			Nonce: nonce,
		})
	})

	state.ForEachCode(func(addr common.Address, code []byte) {
		snap.Code = append(snap.Code, SnapshotCodeEntry{
			Addr: addr,
			Code: append([]byte(nil), code...),
		})
	})

	// Atomic write: write to temp file in same directory, fsync, rename.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".snapshot-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	enc := gob.NewEncoder(tmp)
	if err := enc.Encode(&snap); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("gob encode: %w", err)
	}

	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("fsync: %w", err)
	}

	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}

// ─── LoadSnapshot ───────────────────────────────────────────────────

// LoadSnapshot reads and decodes a snapshot from the given path.
func LoadSnapshot(path string) (*Snapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open snapshot: %w", err)
	}
	defer f.Close()

	var snap Snapshot
	dec := gob.NewDecoder(f)
	if err := dec.Decode(&snap); err != nil {
		return nil, fmt.Errorf("gob decode: %w", err)
	}

	return &snap, nil
}

// ─── ApplySnapshot ──────────────────────────────────────────────────

// ApplySnapshot populates a VersionedState from a snapshot. Each entry is
// inserted at the snapshot's block number.
func ApplySnapshot(state *VersionedState, snap *Snapshot) {
	block := snap.BlockNumber

	for _, e := range snap.Storage {
		state.SetStorage(e.Addr, e.Slot, e.Value, block)
	}

	for _, e := range snap.Balances {
		bal := new(uint256.Int).SetBytes32(e.Balance[:])
		state.SetBalance(e.Addr, bal, block)
	}

	for _, e := range snap.Nonces {
		state.SetNonce(e.Addr, e.Nonce, block)
	}

	for _, e := range snap.Code {
		state.SetCode(e.Addr, e.Code, block)
	}

	state.SetLatestBlock(block)
}
