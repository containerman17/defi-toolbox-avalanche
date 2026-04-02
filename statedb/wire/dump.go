// Package wire defines the gob+zstd wire format for state server initial dumps.
//
// This package uses only primitive types ([20]byte for addresses, [32]byte for
// hashes/balances) so it can be imported by both the state server and statedb
// client without pulling in libevm or uint256 dependencies.
//
// The initial_dump is sent as a single gob-encoded, zstd-compressed GobDump
// over a WebSocket binary frame. Block diffs and JSON-RPC stay as JSON text frames.
package wire

import (
	"bytes"
	"encoding/gob"
	"io"

	"github.com/klauspost/compress/zstd"
)

// GobDump is the gob-encoded wire format for the initial_dump message.
type GobDump struct {
	BlockNumber uint64
	Timestamp   uint64
	BaseFee     uint64
	GasLimit    uint64

	// Storage is a flat slice of (addr, slot, value) triples.
	// Flat slices encode more efficiently in gob than nested maps.
	// The client builds its own map[Address]map[Hash]Hash on decode.
	Storage []StorageEntry

	// Accounts holds balance, nonce, and code for each known address.
	Accounts []AccountEntry
}

// StorageEntry is a single storage slot: contract address + slot + value.
type StorageEntry struct {
	Addr  [20]byte
	Slot  [32]byte
	Value [32]byte
}

// AccountEntry is a single account: address + balance + nonce + code.
// Balance is uint256 big-endian (compatible with uint256.Int.Bytes32/SetBytes32).
// Code is nil or empty for EOAs.
type AccountEntry struct {
	Addr    [20]byte
	Balance [32]byte
	Nonce   uint64
	Code    []byte
}

// Encode gob-encodes a GobDump, then zstd-compresses it to the writer.
func Encode(w io.Writer, d *GobDump) error {
	var gobBuf bytes.Buffer
	if err := gob.NewEncoder(&gobBuf).Encode(d); err != nil {
		return err
	}
	enc, err := zstd.NewWriter(w)
	if err != nil {
		return err
	}
	if _, err := enc.Write(gobBuf.Bytes()); err != nil {
		enc.Close()
		return err
	}
	return enc.Close()
}

// Decode zstd-decompresses, then gob-decodes a GobDump from the reader.
func Decode(r io.Reader) (*GobDump, error) {
	dec, err := zstd.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	var d GobDump
	if err := gob.NewDecoder(dec).Decode(&d); err != nil {
		return nil, err
	}
	return &d, nil
}
