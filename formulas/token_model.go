package formulas

import (
	"math/big"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
	"github.com/holiman/uint256"
)

// solKeccak256Slot returns keccak256(slot) as a 32-byte slice.
// Used to compute the base storage location for Solidity dynamic arrays.
func solKeccak256Slot(slot common.Hash) []byte {
	return crypto.Keccak256(slot[:])
}

// solMappingSlot computes the storage slot for mapping[addr] at base slot:
// keccak256(addr_padded_to_32 ++ slot).
func solMappingSlot(addr common.Address, slot common.Hash) common.Hash {
	var buf [64]byte
	copy(buf[12:32], addr[:])
	copy(buf[32:64], slot[:])
	return crypto.Keccak256Hash(buf[:])
}

// TokenModel describes how a token adjusts transfer amounts.
// Normal ERC20 tokens use the identity (no adjustment).
// Fee-on-transfer tokens subtract a tax from the transferred amount.
type TokenModel interface {
	// AdjustInput returns the effective amount a pool receives after transfer tax.
	AdjustInput(amount *uint256.Int) uint256.Int

	// AdjustOutput returns the effective amount a user receives after transfer tax.
	AdjustOutput(amount *uint256.Int) uint256.Int

	// IsFoT returns true if this model applies a fee-on-transfer adjustment.
	IsFoT() bool
}

// defaultTokenModel is the identity — no adjustment, used for normal ERC20 tokens.
type defaultTokenModel struct{}

func (d defaultTokenModel) AdjustInput(amount *uint256.Int) uint256.Int  { return *amount }
func (d defaultTokenModel) AdjustOutput(amount *uint256.Int) uint256.Int { return *amount }
func (d defaultTokenModel) IsFoT() bool                                  { return false }

// fotTokenModel subtracts a fee-on-transfer tax from the amount.
type fotTokenModel struct {
	calcFee      func(*big.Int) *big.Int
	calcReceived func(*big.Int) *big.Int // if set, computes received amount directly (matches Solidity rounding)
}

func (f *fotTokenModel) AdjustInput(amount *uint256.Int) uint256.Int {
	return f.adjust(amount)
}

func (f *fotTokenModel) AdjustOutput(amount *uint256.Int) uint256.Int {
	return f.adjust(amount)
}

func (f *fotTokenModel) IsFoT() bool { return true }

// adjust computes the post-fee amount. If calcReceived is set, use it directly
// (matches Solidity's `amount * (10000 - fee) / 10000` pattern without rounding error).
// Otherwise falls back to `amount - calcFee(amount)`.
func (f *fotTokenModel) adjust(amount *uint256.Int) uint256.Int {
	amtBig := amount.ToBig()
	var resultBig *big.Int

	if f.calcReceived != nil {
		resultBig = f.calcReceived(amtBig)
	} else {
		fee := f.calcFee(amtBig)
		resultBig = new(big.Int).Sub(amtBig, fee)
	}

	if resultBig.Sign() <= 0 {
		return uint256.Int{}
	}
	adjusted, overflow := uint256.FromBig(resultBig)
	if overflow || adjusted.IsZero() {
		return uint256.Int{}
	}
	return *adjusted
}

// SenderAwareOutputAdjuster is optionally implemented by TokenModels that need
// the sender address to compute the exact output. Reflection tokens use this to
// adjust for excluded senders, whose _rOwned/_tOwned changes affect _getCurrentSupply.
type SenderAwareOutputAdjuster interface {
	AdjustOutputFromSender(amount *uint256.Int, sender common.Address) uint256.Int
}

// RecipientAwareInputAdjuster is optionally implemented by TokenModels that need
// the recipient (pool) address to compute the exact input. Reflection tokens use this
// because _reflectFee changes the rate, giving the recipient a bonus on their existing
// rOwned balance. The V2 router measures balanceOf(pool) after - before, which includes
// this bonus. Without the recipient's address, AdjustInput under-estimates the effective
// input for pools with large existing balances.
type RecipientAwareInputAdjuster interface {
	AdjustInputToRecipient(amount *uint256.Int, recipient common.Address) uint256.Int
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

	// Excluded account support: RFI _getRate() subtracts excluded accounts from supply.
	// If excludedArraySlot is non-zero, getCurrentSupply reads the _excluded array and
	// subtracts each account's _rOwned/_tOwned from rTotal/tTotal.
	excludedArraySlot common.Hash // storage slot for _excluded dynamic array (0 = no exclusions)
	rOwnedSlot        common.Hash // storage slot for _rOwned mapping
	tOwnedSlot        common.Hash // storage slot for _tOwned mapping
}

func (r *reflectionTokenModel) IsFoT() bool { return true }

func (r *reflectionTokenModel) AdjustInput(amount *uint256.Int) uint256.Int {
	return r.adjustReflection(amount, common.Address{})
}

func (r *reflectionTokenModel) AdjustOutput(amount *uint256.Int) uint256.Int {
	return r.adjustReflection(amount, common.Address{})
}

// AdjustInputToRecipient computes the effective balance increase of the recipient (pool)
// after a transfer of `amount` tokens. This accounts for the reflection bonus on the
// recipient's existing rOwned balance caused by _reflectFee reducing rTotal.
// actualIn = balanceOf(recipient) after - balanceOf(recipient) before
//          = (recipientROwned + rTransferAmount) / newRate - recipientROwned / oldRate
func (r *reflectionTokenModel) AdjustInputToRecipient(amount *uint256.Int, recipient common.Address) uint256.Int {
	if (r.rOwnedSlot == common.Hash{}) {
		return r.adjustReflection(amount, common.Address{})
	}
	tAmount := amount.ToBig()
	totalFee := r.calcFee(tAmount)
	tTransfer := new(big.Int).Sub(tAmount, totalFee)
	if tTransfer.Sign() <= 0 {
		return uint256.Int{}
	}

	// Reflection fee only
	tFee := new(big.Int).Mul(tAmount, big.NewInt(r.reflectRate))
	tFee.Div(tFee, big.NewInt(r.reflectDenom))

	// Burn fee if applicable
	var tBurn *big.Int
	if r.burnRate > 0 {
		tBurn = new(big.Int).Mul(tAmount, big.NewInt(r.burnRate))
		tBurn.Div(tBurn, big.NewInt(r.burnDenom))
	}

	// Read _rTotal
	rTotalRaw := r.reader(r.tokenAddr, r.rTotalSlot)
	rTotal := new(big.Int).SetBytes(rTotalRaw[:])
	if rTotal.Sign() == 0 {
		result, _ := uint256.FromBig(tTransfer)
		if result == nil { return uint256.Int{} }
		return *result
	}

	// Read _tTotal
	tTotal := r.tTotal
	if (r.tTotalSlot != common.Hash{}) {
		tTotalRaw := r.reader(r.tokenAddr, r.tTotalSlot)
		tTotal = new(big.Int).SetBytes(tTotalRaw[:])
		if tTotal.Sign() == 0 {
			result, _ := uint256.FromBig(tTransfer)
			if result == nil { return uint256.Int{} }
			return *result
		}
	}

	rSupply, tSupply := r.getCurrentSupply(rTotal, tTotal)
	if tSupply.Sign() <= 0 {
		result, _ := uint256.FromBig(tTransfer)
		if result == nil { return uint256.Int{} }
		return *result
	}

	oldRate := new(big.Int).Div(rSupply, tSupply)
	if oldRate.Sign() <= 0 {
		result, _ := uint256.FromBig(tTransfer)
		if result == nil { return uint256.Int{} }
		return *result
	}

	rFee := new(big.Int).Mul(tFee, oldRate)
	rTransferAmount := new(big.Int).Mul(tTransfer, oldRate)

	newRSupply := new(big.Int).Sub(rSupply, rFee)
	newTSupply := new(big.Int).Set(tSupply)
	if tBurn != nil {
		rBurn := new(big.Int).Mul(tBurn, oldRate)
		newRSupply.Sub(newRSupply, rBurn)
		newTSupply.Sub(newTSupply, tBurn)
	}

	if newRSupply.Sign() <= 0 || newTSupply.Sign() <= 0 {
		result, _ := uint256.FromBig(tTransfer)
		if result == nil { return uint256.Int{} }
		return *result
	}

	newRate := new(big.Int).Div(newRSupply, newTSupply)
	if newRate.Sign() <= 0 {
		result, _ := uint256.FromBig(tTransfer)
		if result == nil { return uint256.Int{} }
		return *result
	}

	// Read recipient's _rOwned
	recipientROwnedRaw := r.reader(r.tokenAddr, solMappingSlot(recipient, r.rOwnedSlot))
	recipientROwned := new(big.Int).SetBytes(recipientROwnedRaw[:])

	// balBefore = recipientROwned / oldRate
	balBefore := new(big.Int).Div(recipientROwned, oldRate)
	// balAfter = (recipientROwned + rTransferAmount) / newRate
	balAfter := new(big.Int).Add(recipientROwned, rTransferAmount)
	balAfter.Div(balAfter, newRate)

	actualIn := new(big.Int).Sub(balAfter, balBefore)
	if actualIn.Sign() <= 0 {
		return uint256.Int{}
	}

	result, overflow := uint256.FromBig(actualIn)
	if overflow || result == nil || result.IsZero() {
		return uint256.Int{}
	}
	return *result
}

func (r *reflectionTokenModel) AdjustOutputFromSender(amount *uint256.Int, sender common.Address) uint256.Int {
	return r.adjustReflection(amount, sender)
}

// getCurrentSupply mirrors Solidity's _getCurrentSupply(), which subtracts
// excluded accounts' _rOwned and _tOwned from the raw totals.
// Returns (rSupply, tSupply). If no excluded accounts are configured or the
// array is empty, returns (rTotal, tTotal) unchanged.
func (r *reflectionTokenModel) getCurrentSupply(rTotal, tTotal *big.Int) (rSupply, tSupply *big.Int) {
	if (r.excludedArraySlot == common.Hash{}) {
		return rTotal, tTotal
	}

	// Read _excluded.length from the array slot
	lengthRaw := r.reader(r.tokenAddr, r.excludedArraySlot)
	length := new(big.Int).SetBytes(lengthRaw[:])
	if length.Sign() == 0 || length.BitLen() > 16 {
		// No excluded accounts or absurdly large (safety cap)
		return rTotal, tTotal
	}
	n := int(length.Int64())

	// Array elements start at keccak256(slot)
	arrayBase := new(big.Int).SetBytes(solKeccak256Slot(r.excludedArraySlot))

	rSupply = new(big.Int).Set(rTotal)
	tSupply = new(big.Int).Set(tTotal)

	for i := 0; i < n; i++ {
		// Read excluded address from array element
		elemSlot := new(big.Int).Add(arrayBase, big.NewInt(int64(i)))
		var elemSlotHash common.Hash
		elemSlot.FillBytes(elemSlotHash[:])
		addrRaw := r.reader(r.tokenAddr, elemSlotHash)
		var excAddr common.Address
		copy(excAddr[:], addrRaw[12:32]) // address is right-aligned in 32-byte slot

		// Read _rOwned[excAddr]: keccak256(addr_padded ++ rOwnedSlot)
		rOwnedHash := r.reader(r.tokenAddr, solMappingSlot(excAddr, r.rOwnedSlot))
		rOwned := new(big.Int).SetBytes(rOwnedHash[:])

		// Read _tOwned[excAddr]: keccak256(addr_padded ++ tOwnedSlot)
		tOwnedHash := r.reader(r.tokenAddr, solMappingSlot(excAddr, r.tOwnedSlot))
		tOwned := new(big.Int).SetBytes(tOwnedHash[:])

		// Solidity safety: if any excluded account exceeds supply, return raw totals
		if rOwned.Cmp(rSupply) > 0 || tOwned.Cmp(tSupply) > 0 {
			return rTotal, tTotal
		}

		rSupply.Sub(rSupply, rOwned)
		tSupply.Sub(tSupply, tOwned)
	}

	// Solidity safety: if rSupply < rTotal/tTotal, return raw totals
	minR := new(big.Int).Div(rTotal, tTotal)
	if rSupply.Cmp(minR) < 0 {
		return rTotal, tTotal
	}

	return rSupply, tSupply
}

// adjustReflection computes the exact post-reflection received amount.
// tAmount is the raw transfer amount before any fees.
// sender is the transfer sender (e.g. the V2 pool); if non-zero AND the sender is
// an excluded account, the post-transfer _getCurrentSupply is adjusted for the
// sender's _rOwned/_tOwned changes (rOwned -= rAmount, tOwned -= tAmount).
// Returns the amount the recipient's balanceOf increases by.
func (r *reflectionTokenModel) adjustReflection(amount *uint256.Int, sender common.Address) uint256.Int {
	tAmount := amount.ToBig()

	// Compute total fee (reflection + team/other) — same as fotCalculators entry
	totalFee := r.calcFee(tAmount)
	tTransfer := new(big.Int).Sub(tAmount, totalFee)
	if tTransfer.Sign() <= 0 {
		return uint256.Int{}
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
		if result == nil { return uint256.Int{} }
		return *result
	}

	// Read _tTotal from storage if slot is set, otherwise use constant
	tTotal := r.tTotal
	if (r.tTotalSlot != common.Hash{}) {
		tTotalRaw := r.reader(r.tokenAddr, r.tTotalSlot)
		tTotal = new(big.Int).SetBytes(tTotalRaw[:])
		if tTotal.Sign() == 0 {
			result, _ := uint256.FromBig(tTransfer)
			if result == nil { return uint256.Int{} }
			return *result
		}
	}

	// Compute effective supply: _getCurrentSupply() subtracts excluded accounts
	rSupply, tSupply := r.getCurrentSupply(rTotal, tTotal)

	// rate = rSupply / tSupply (integer division, same as Solidity _getRate)
	rate := new(big.Int).Div(rSupply, tSupply)

	// rAmount = tAmount * rate (total r-space amount debited from sender)
	rAmount := new(big.Int).Mul(tAmount, rate)

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

	// After _reflectFee: _rTotal -= rFee (and rBurn if applicable).
	// This changes the effective supply for the new rate calculation.
	// For non-excluded senders: rSupply decreases by rFee only.
	// For excluded senders: _rOwned[sender] -= rAmount and _tOwned[sender] -= tAmount,
	// so rSupply = (rTotal - rFee) - (sender_rOwned - rAmount) - other_excluded
	//            = rSupply - rFee + rAmount
	// and tSupply = tTotal - (sender_tOwned - tAmount) - other_excluded
	//            = tSupply + tAmount
	newRSupply := new(big.Int).Sub(rSupply, rFee)
	newTSupply := new(big.Int).Set(tSupply)
	if rBurn != nil {
		newRSupply.Sub(newRSupply, rBurn)
		newTSupply.Sub(newTSupply, tBurn)
	}

	// If sender is excluded, adjust for their _rOwned/_tOwned changes
	if (sender != common.Address{}) && (r.excludedArraySlot != common.Hash{}) {
		if r.isSenderExcluded(sender) {
			newRSupply.Add(newRSupply, rAmount)
			newTSupply.Add(newTSupply, tAmount)
		}
	}

	if newRSupply.Sign() <= 0 || newTSupply.Sign() <= 0 {
		result, _ := uint256.FromBig(tTransfer)
		if result == nil { return uint256.Int{} }
		return *result
	}

	// newRate = newRSupply / newTSupply
	newRate := new(big.Int).Div(newRSupply, newTSupply)
	if newRate.Sign() <= 0 {
		result, _ := uint256.FromBig(tTransfer)
		if result == nil { return uint256.Int{} }
		return *result
	}

	// buyer_t = rTransferAmount / newRate (integer division, same as Solidity balanceOf)
	buyerT := new(big.Int).Div(rTransferAmount, newRate)

	result, overflow := uint256.FromBig(buyerT)
	if overflow || result == nil || result.IsZero() {
		return uint256.Int{}
	}
	return *result
}

// isSenderExcluded checks if the sender address is in the _excluded array.
func (r *reflectionTokenModel) isSenderExcluded(sender common.Address) bool {
	if (r.excludedArraySlot == common.Hash{}) {
		return false
	}
	lengthRaw := r.reader(r.tokenAddr, r.excludedArraySlot)
	length := new(big.Int).SetBytes(lengthRaw[:])
	if length.Sign() == 0 || length.BitLen() > 16 {
		return false
	}
	n := int(length.Int64())
	arrayBase := new(big.Int).SetBytes(solKeccak256Slot(r.excludedArraySlot))
	for i := 0; i < n; i++ {
		elemSlot := new(big.Int).Add(arrayBase, big.NewInt(int64(i)))
		var elemSlotHash common.Hash
		elemSlot.FillBytes(elemSlotHash[:])
		addrRaw := r.reader(r.tokenAddr, elemSlotHash)
		var excAddr common.Address
		copy(excAddr[:], addrRaw[12:32])
		if excAddr == sender {
			return true
		}
	}
	return false
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
				tokenAddr:         common.HexToAddress(addr),
				reader:            reader,
				rTotalSlot:        cfg.rTotalSlot,
				tTotalSlot:        cfg.tTotalSlot,
				tTotal:            cfg.tTotal,
				reflectRate:       cfg.reflectRate,
				reflectDenom:      cfg.reflectDenom,
				burnRate:          cfg.burnRate,
				burnDenom:         cfg.burnDenom,
				calcFee:           cfg.calcFee,
				excludedArraySlot: cfg.excludedArraySlot,
				rOwnedSlot:        cfg.rOwnedSlot,
				tOwnedSlot:        cfg.tOwnedSlot,
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
