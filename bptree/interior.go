package bptree

import "encoding/binary"

// InteriorEntry is one separator record on an interior page. Children satisfy:
//
//	subtree(LeftChild) keys < Sep <= subtree(RightChild) keys.
//
// The first child pointer lives in the page header (offLink); records are
// ordered by their shortest separators.
type InteriorEntry struct {
	Sep       []byte
	RightPage int
}

// interiorEncode re-encodes an interior page. Separators are stored as
// shortest separators of the actual subtree boundary keys (computed by the
// tree before calling this function), and additionally share the page's own
// common prefix.
func interiorEncode(pg Pager, seps []InteriorEntry, firstChild int) ([]byte, error) {
	keys := make([][]byte, len(seps))
	for i := range seps {
		keys[i] = seps[i].Sep
	}
	cpLen := commonPrefixAll(keys)
	var cp []byte
	if len(seps) > 0 {
		cp = append([]byte(nil), seps[0].Sep[:cpLen]...)
	}
	b := newPageBuilder(pg.PageSize(), pageInterior, cp, firstChild, 0)
	for i := range seps {
		suffix := seps[i].Sep[cpLen:]
		rec := make([]byte, 0, len(suffix)+binary.MaxVarintLen64)
		rec = binary.AppendUvarint(rec, uint64(len(suffix)))
		rec = append(rec, suffix...)
		rec = binary.AppendUvarint(rec, uint64(seps[i].RightPage))
		if err := b.addRecord(rec); err != nil {
			return nil, err
		}
	}
	return b.finish(uint16(len(seps)))
}

// interiorDecode decodes an interior page into its first child and separators.
func interiorDecode(pg Pager, buf []byte) (firstChild int, seps []InteriorEntry, cp []byte) {
	n := int(u16(buf, offNKeys))
	rstart := int(u16(buf, offRStart))
	cpLen := int(u16(buf, offCPLen))
	cp = append([]byte(nil), buf[headerSize:headerSize+cpLen]...)
	firstChild = int(u64(buf, offLink))
	seps = make([]InteriorEntry, 0, n)
	for i := 0; i < n; i++ {
		off := int(u16(buf, rstart+i*slotSize))
		ln := int(u16(buf, rstart+i*slotSize+2))
		rec := buf[off : off+ln]
		slen, k := binary.Uvarint(rec)
		suffix := rec[k : k+int(slen)]
		child, k2 := binary.Uvarint(rec[k+int(slen):])
		_ = k2
		sep := make([]byte, 0, cpLen+len(suffix))
		sep = append(sep, cp...)
		sep = append(sep, suffix...)
		seps = append(seps, InteriorEntry{Sep: sep, RightPage: int(child)})
	}
	return firstChild, seps, cp
}

// interiorSearch returns the child page that may contain key: the first
// child while key < the first separator, otherwise the child right after the
// last separator <= key. The binary search works on full separator suffixes
// (interior records are not delta encoded) and only compares from the first
// differing byte via compareKeySuffix.
func interiorSearch(buf, key []byte) int {
	n := int(u16(buf, offNKeys))
	cpLen := int(u16(buf, offCPLen))
	cp := buf[headerSize : headerSize+cpLen]
	rstart := int(u16(buf, offRStart))
	firstChild := int(u64(buf, offLink))

	suffixAt := func(i int) []byte {
		off := int(u16(buf, rstart+i*slotSize))
		slen, k := binary.Uvarint(buf[off:])
		return buf[off+k : off+k+int(slen)]
	}

	// Upper-bound binary search: first slot whose separator > key.
	// compareKeySuffix(cp, suffix, key) returns key's order vs the separator;
	// the separator is <= key exactly when that result is >= 0.
	lo, hi := 0, n
	for lo < hi {
		mid := (lo + hi) / 2
		if compareKeySuffix(cp, suffixAt(mid), key) >= 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return firstChild
	}
	off := int(u16(buf, rstart+(lo-1)*slotSize))
	ln := int(u16(buf, rstart+(lo-1)*slotSize+2))
	rec := buf[off : off+ln]
	slen, k := binary.Uvarint(rec)
	child, _ := binary.Uvarint(rec[k+int(slen):])
	return int(child)
}
