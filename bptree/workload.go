package bptree

import (
	"fmt"
	"math/rand"
)

// Part-code layout used by the workload generator. The first prefixBytes
// bytes are a small-vocabulary brand/model/year block so generated keys share
// long prefixes (the watch-repair inventory shape), followed by a unique
// serial tail.
var partBrands = []string{"ROLEX", "OMEGA", "SEIKO", "TISSOT", "CASIO", "CITIZN"}
var partModels = []string{"SUBM", "SPDM", "DITN", "AQUA", "GSHK", "PRX5", "MOON", "KING"}

// GenPartKey is the exported wrapper used by the bptool demo workload.
func GenPartKey(rng *rand.Rand, serial int) []byte {
	return genPartKey(rng, serial, false)
}

// genPartKey builds one fixed-format part code:
//
//	BRAND(6) MODEL(5) YEAR(4) BATCH(4) SERIAL(10)  -> 29+ bytes, zero padded.
func genPartKey(rng *rand.Rand, serial int, longTail bool) []byte {
	b := make([]byte, 0, 64)
	b = append(b, fmt.Sprintf("%-6s", partBrands[rng.Intn(len(partBrands))])...)
	b = append(b, fmt.Sprintf("%-5s", partModels[rng.Intn(len(partModels))])...)
	b = append(b, fmt.Sprintf("%04d", 2010+rng.Intn(16))...)
	b = append(b, fmt.Sprintf("%04d", rng.Intn(50))...)
	b = append(b, fmt.Sprintf("%010d", serial)...)
	if longTail {
		// Push total length past pageSize/4 in tests using small pages, so
		// the overflow-chain path (requirement 5) is exercised.
		b = append(b, make([]byte, 400)...)
	}
	return b
}

// refModel is the sorted-array oracle every tree result is compared against.
type refModel struct {
	keys   [][]byte
	values [][]byte
}

func (m *refModel) index(key []byte) int {
	lo, hi := 0, len(m.keys)
	for lo < hi {
		mid := (lo + hi) / 2
		if compareBytes(m.keys[mid], key) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

func (m *refModel) put(key, value []byte) {
	i := m.index(key)
	if i < len(m.keys) && compareBytes(m.keys[i], key) == 0 {
		m.values[i] = append([]byte(nil), value...)
		return
	}
	m.keys = append(m.keys, nil)
	copy(m.keys[i+1:], m.keys[i:])
	m.keys[i] = append([]byte(nil), key...)
	m.values = append(m.values, nil)
	copy(m.values[i+1:], m.values[i:])
	m.values[i] = append([]byte(nil), value...)
}

func (m *refModel) delete(key []byte) bool {
	i := m.index(key)
	if i >= len(m.keys) || compareBytes(m.keys[i], key) != 0 {
		return false
	}
	copy(m.keys[i:], m.keys[i+1:])
	copy(m.values[i:], m.values[i+1:])
	m.keys = m.keys[:len(m.keys)-1]
	m.values = m.values[:len(m.values)-1]
	return true
}

func (m *refModel) get(key []byte) ([]byte, bool) {
	i := m.index(key)
	if i >= len(m.keys) || compareBytes(m.keys[i], key) != 0 {
		return nil, false
	}
	return m.values[i], true
}

// scan returns reference pairs in [low, high).
func (m *refModel) scan(low, high []byte) []KeyValue {
	i := 0
	if low != nil {
		i = m.index(low)
	}
	var out []KeyValue
	for ; i < len(m.keys); i++ {
		if high != nil && compareBytes(m.keys[i], high) >= 0 {
			break
		}
		out = append(out, KeyValue{
			Key:   append([]byte(nil), m.keys[i]...),
			Value: append([]byte(nil), m.values[i]...),
		})
	}
	return out
}
