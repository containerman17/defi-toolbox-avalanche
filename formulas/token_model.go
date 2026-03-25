package formulas

import (
	"math/big"

	"github.com/holiman/uint256"
)

// TokenModel describes how a token adjusts transfer amounts.
// Normal ERC20 tokens use the identity (no adjustment).
// Fee-on-transfer tokens subtract a tax from the transferred amount.
type TokenModel interface {
	// AdjustInput returns the effective amount a pool receives after transfer tax.
	AdjustInput(amount *uint256.Int) *uint256.Int

	// AdjustOutput returns the effective amount a user receives after transfer tax.
	AdjustOutput(amount *uint256.Int) *uint256.Int

	// IsFoT returns true if this model applies a fee-on-transfer adjustment.
	IsFoT() bool
}

// defaultTokenModel is the identity — no adjustment, used for normal ERC20 tokens.
type defaultTokenModel struct{}

func (d defaultTokenModel) AdjustInput(amount *uint256.Int) *uint256.Int  { return amount }
func (d defaultTokenModel) AdjustOutput(amount *uint256.Int) *uint256.Int { return amount }
func (d defaultTokenModel) IsFoT() bool                                   { return false }

// fotTokenModel subtracts a fee-on-transfer tax from the amount.
type fotTokenModel struct {
	calcFee func(*big.Int) *big.Int
}

func (f *fotTokenModel) AdjustInput(amount *uint256.Int) *uint256.Int {
	return f.adjust(amount)
}

func (f *fotTokenModel) AdjustOutput(amount *uint256.Int) *uint256.Int {
	return f.adjust(amount)
}

func (f *fotTokenModel) IsFoT() bool { return true }

// adjust subtracts the fee from amount. Returns nil if the result is zero or negative.
func (f *fotTokenModel) adjust(amount *uint256.Int) *uint256.Int {
	fee := f.calcFee(amount.ToBig())
	feeU256, overflow := uint256.FromBig(fee)
	if overflow {
		return nil
	}
	adjusted := new(uint256.Int).Sub(amount, feeU256)
	if adjusted.IsZero() || adjusted.Sign() < 0 {
		return nil
	}
	return adjusted
}

// TokenModelRegistry maps token addresses to their TokenModel.
type TokenModelRegistry struct {
	models       map[string]TokenModel // lowercase hex address → model
	defaultModel TokenModel
}

// NewTokenModelRegistry creates a registry pre-loaded from the fotCalculators in fot.go.
func NewTokenModelRegistry() *TokenModelRegistry {
	r := &TokenModelRegistry{
		models:       make(map[string]TokenModel, len(fotCalculators)),
		defaultModel: defaultTokenModel{},
	}
	for addr, calc := range fotCalculators {
		r.models[addr] = &fotTokenModel{calcFee: calc}
	}
	return r
}

// GetModel returns the TokenModel for a token address (lowercase hex).
// Returns the default identity model if the token has no special behavior.
func (r *TokenModelRegistry) GetModel(addr string) TokenModel {
	if m, ok := r.models[addr]; ok {
		return m
	}
	return r.defaultModel
}
