package formulas

import (
	"math/big"

	"github.com/ava-labs/libevm/common"
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
	calcFee      func(*big.Int) *big.Int
	calcReceived func(*big.Int) *big.Int // if set, computes received amount directly (matches Solidity rounding)
}

func (f *fotTokenModel) AdjustInput(amount *uint256.Int) *uint256.Int {
	return f.adjust(amount)
}

func (f *fotTokenModel) AdjustOutput(amount *uint256.Int) *uint256.Int {
	return f.adjust(amount)
}

func (f *fotTokenModel) IsFoT() bool { return true }

// adjust computes the post-fee amount. If calcReceived is set, use it directly
// (matches Solidity's `amount * (10000 - fee) / 10000` pattern without rounding error).
// Otherwise falls back to `amount - calcFee(amount)`.
func (f *fotTokenModel) adjust(amount *uint256.Int) *uint256.Int {
	amtBig := amount.ToBig()
	var resultBig *big.Int

	if f.calcReceived != nil {
		resultBig = f.calcReceived(amtBig)
	} else {
		fee := f.calcFee(amtBig)
		resultBig = new(big.Int).Sub(amtBig, fee)
	}

	if resultBig.Sign() <= 0 {
		return nil
	}
	adjusted, overflow := uint256.FromBig(resultBig)
	if overflow || adjusted.IsZero() {
		return nil
	}
	return adjusted
}

// reflectionTokenModel implements exact RFI/SafeMoon reflection math.
// After a transfer, _reflectFee reduces _rTotal by rFee, changing the rate.
// The recipient's actual received tokens = rTransferAmount * _tTotal / (_rTotal - rFee),
// which is slightly MORE than tTransferAmount. This model reads _rTotal from storage
// at quote time to compute the exact amount.
//
// For tokens with burn (e.g. KIOO/Reflectx), the burn also reduces _rTotal by rBurn
// AND reduces _tTotal by burn. Set burnRate/burnDenom and tTotalSlot for these tokens.
type reflectionTokenModel struct {
	tokenAddr    common.Address
	reader       StorageReader
	rTotalSlot   common.Hash  // storage slot for _rTotal (_reflectSupply)
	tTotalSlot   common.Hash  // storage slot for _tTotal (zero hash = use constant tTotal)
	tTotal       *big.Int     // constant total supply (used when tTotalSlot is zero)
	reflectRate  int64        // numerator for reflection fee (e.g. 1 for 1%)
	reflectDenom int64        // denominator for reflection fee (e.g. 100)
	burnRate     int64        // numerator for burn fee (0 = no burn)
	burnDenom    int64        // denominator for burn fee (0 = no burn)
	calcFee      func(*big.Int) *big.Int // total fee calculator (same as fotCalculators entry)
}

func (r *reflectionTokenModel) IsFoT() bool { return true }

func (r *reflectionTokenModel) AdjustInput(amount *uint256.Int) *uint256.Int {
	return r.adjustReflection(amount)
}

func (r *reflectionTokenModel) AdjustOutput(amount *uint256.Int) *uint256.Int {
	return r.adjustReflection(amount)
}

// adjustReflection computes the exact post-reflection received amount.
// tAmount is the raw transfer amount before any fees.
// Returns the amount the recipient's balanceOf increases by.
func (r *reflectionTokenModel) adjustReflection(amount *uint256.Int) *uint256.Int {
	tAmount := amount.ToBig()

	// Compute total fee (reflection + team/other) — same as fotCalculators entry
	totalFee := r.calcFee(tAmount)
	tTransfer := new(big.Int).Sub(tAmount, totalFee)
	if tTransfer.Sign() <= 0 {
		return nil
	}

	// Compute reflection fee only (the part that reduces _rTotal via _reflectFee)
	tFee := new(big.Int).Mul(tAmount, big.NewInt(r.reflectRate))
	tFee.Div(tFee, big.NewInt(r.reflectDenom))

	// Compute burn fee if applicable (reduces both _rTotal and _tTotal)
	var tBurn *big.Int
	if r.burnRate > 0 {
		tBurn = new(big.Int).Mul(tAmount, big.NewInt(r.burnRate))
		tBurn.Div(tBurn, big.NewInt(r.burnDenom))
	}

	// Read current _rTotal from storage
	rTotalRaw := r.reader(r.tokenAddr, r.rTotalSlot)
	rTotal := new(big.Int).SetBytes(rTotalRaw[:])
	if rTotal.Sign() == 0 {
		// Fallback: if storage read fails, use basic fee subtraction
		result, _ := uint256.FromBig(tTransfer)
		return result
	}

	// Read _tTotal from storage if slot is set, otherwise use constant
	tTotal := r.tTotal
	if (r.tTotalSlot != common.Hash{}) {
		tTotalRaw := r.reader(r.tokenAddr, r.tTotalSlot)
		tTotal = new(big.Int).SetBytes(tTotalRaw[:])
		if tTotal.Sign() == 0 {
			result, _ := uint256.FromBig(tTransfer)
			return result
		}
	}

	// rate = _rTotal / _tTotal (integer division, same as Solidity _getRate)
	rate := new(big.Int).Div(rTotal, tTotal)

	// rFee = tFee * rate (reflection fee in r-space)
	rFee := new(big.Int).Mul(tFee, rate)

	// rBurn = tBurn * rate (burn fee in r-space, if applicable)
	var rBurn *big.Int
	if tBurn != nil {
		rBurn = new(big.Int).Mul(tBurn, rate)
	}

	// rTransferAmount = tTransfer * rate
	// (rAmount - rFee - rBurn - rTeam = tTransfer * rate)
	rTransferAmount := new(big.Int).Mul(tTransfer, rate)

	// After _reflectFeeBurn:
	//   newRTotal = _rTotal - rFee - rBurn (both reduce _reflectSupply)
	//   newTTotal = _tTotal - tBurn        (only burn reduces _totalSupply)
	newRTotal := new(big.Int).Sub(rTotal, rFee)
	newTTotal := new(big.Int).Set(tTotal)
	if rBurn != nil {
		newRTotal.Sub(newRTotal, rBurn)
		newTTotal.Sub(newTTotal, tBurn)
	}
	if newRTotal.Sign() <= 0 || newTTotal.Sign() <= 0 {
		result, _ := uint256.FromBig(tTransfer)
		return result
	}

	// newRate = newRTotal / newTTotal
	newRate := new(big.Int).Div(newRTotal, newTTotal)
	if newRate.Sign() <= 0 {
		result, _ := uint256.FromBig(tTransfer)
		return result
	}

	// buyer_t = rTransferAmount / newRate (integer division, same as Solidity balanceOf)
	buyerT := new(big.Int).Div(rTransferAmount, newRate)

	result, overflow := uint256.FromBig(buyerT)
	if overflow {
		return nil
	}
	if result.IsZero() {
		return nil
	}
	return result
}

// TokenModelRegistry maps token addresses to their TokenModel.
type TokenModelRegistry struct {
	models       map[string]TokenModel // lowercase hex address → model
	defaultModel TokenModel
}

// NewTokenModelRegistry creates a registry pre-loaded from the fotCalculators
// and reflectionTokenConfigs in fot.go. The StorageReader is used by reflection
// token models to read _rTotal at quote time. Pass nil for environments without
// storage access (reflection models will be skipped).
func NewTokenModelRegistry(reader StorageReader) *TokenModelRegistry {
	r := &TokenModelRegistry{
		models:       make(map[string]TokenModel, len(fotCalculators)+len(reflectionTokenConfigs)),
		defaultModel: defaultTokenModel{},
	}
	for addr, calc := range fotCalculators {
		r.models[addr] = &fotTokenModel{calcFee: calc.calcFee, calcReceived: calc.calcReceived}
	}
	// Register reflection token models (override any fotCalculators entry for same address)
	if reader != nil {
		for addr, cfg := range reflectionTokenConfigs {
			r.models[addr] = &reflectionTokenModel{
				tokenAddr:    common.HexToAddress(addr),
				reader:       reader,
				rTotalSlot:   cfg.rTotalSlot,
				tTotalSlot:   cfg.tTotalSlot,
				tTotal:       cfg.tTotal,
				reflectRate:  cfg.reflectRate,
				reflectDenom: cfg.reflectDenom,
				burnRate:     cfg.burnRate,
				burnDenom:    cfg.burnDenom,
				calcFee:      cfg.calcFee,
			}
		}
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
