package formulas

import "math/big"

func algebraDecodeInt24(data []byte) int32 {
	v := int32(0)
	for _, b := range data {
		v = v<<8 | int32(b)
	}
	if v&0x800000 != 0 {
		v -= 0x1000000
	}
	return v
}

func algebraEncodeInt24(tick int32) []byte {
	v := uint32(tick)
	return []byte{byte(v >> 16), byte(v >> 8), byte(v)}
}

func algebraDecodeInt128(data []byte) *big.Int {
	v := new(big.Int).SetBytes(data)
	if v.Bit(127) == 1 {
		v.Sub(v, new(big.Int).Lsh(big.NewInt(1), 128))
	}
	return v
}
