package bptree

import "encoding/binary"

// LeafEntry is the in-memory representation of one leaf record.
//
// When a key is longer than a quarter page (requirement 5), KeyOV points at an
// overflow chain holding the FULL key while Key carries only the truncated
// inline routing prefix fullKey[:maxInline]. Searches whose target ties the
// inline prefix read the chain to disambiguate. ValOV likewise offloads an
// over-long value.
type LeafEntry struct {
	Key   []byte
	Value []byte
	KeyOV int
	ValOV int
}

// leafEncode re-encodes a full leaf page. It is the single place that touches
// leaf layout, so:
//   - inserting a key that shortens the page common prefix simply decodes
//     the page, mutates the entry list and re-encodes (requirement 1);
//   - deleting an entry and then re-encoding reclaims all dead space, which
//     is exactly the page compaction on a full page (requirement 4).
//
// The returned error is ErrPageFull when the entries cannot physically fit.
func leafEncode(pg Pager, entries []LeafEntry, prev, next int) ([]byte, error) {
	pageSize := pg.PageSize()
	maxInline := maxInlineLen(pageSize)

	// Truncate over-long keys first; the page common prefix is computed over
	// the stored inline keys so it can never run past the truncation point.
	stored := make([][]byte, len(entries))
	for i := range entries {
		if len(entries[i].Key) > maxInline {
			stored[i] = entries[i].Key[:maxInline]
		} else {
			stored[i] = entries[i].Key
		}
	}
	cpLen := commonPrefixAll(stored)
	var cp []byte
	if len(stored) > 0 {
		cp = append([]byte(nil), stored[0][:cpLen]...)
	}

	// newPageBuilder's link1 lands in offLink (next sibling), link2 in
	// offPrev (previous sibling).
	b := newPageBuilder(pageSize, pageLeaf, cp, next, prev)
	var prevSuffix []byte

	for i := range entries {
		e := &entries[i]
		fullKey := e.Key
		flags := byte(0)

		keyOV := 0
		if len(fullKey) > maxInline {
			keyOV = allocChain(pg, fullKey) // full key on the overflow chain
			flags |= flagKeyOverflow
		}
		suffix := stored[i][cpLen:]

		// Requirement 6: restart points fall at fixed slot positions
		// (i % restartInterval == 0) so the binary search can index them
		// directly. An overflow record additionally carries its full inline
		// suffix, but does not shift the restart cadence.
		restart := i%restartInterval == 0
		var shared int
		if restart {
			flags |= flagRestart
		} else {
			shared = commonPrefixLen(prevSuffix, suffix)
		}

		valOV := 0
		inlineVal := e.Value
		if len(e.Value) > maxInline {
			valOV = allocChain(pg, e.Value)
			inlineVal = nil
			flags |= flagValOverflow
		}

		rec := make([]byte, 0, 16+len(suffix)+len(inlineVal))
		rec = append(rec, flags)
		if restart {
			rec = binary.AppendUvarint(rec, uint64(len(suffix)))
			rec = append(rec, suffix...)
		} else {
			rec = binary.AppendUvarint(rec, uint64(shared))
			rec = binary.AppendUvarint(rec, uint64(len(suffix)-shared))
			rec = append(rec, suffix[shared:]...)
		}
		if keyOV != 0 {
			rec = binary.AppendUvarint(rec, uint64(keyOV))
		}
		if valOV != 0 {
			rec = binary.AppendUvarint(rec, uint64(valOV))
		} else {
			rec = binary.AppendUvarint(rec, uint64(len(inlineVal)))
			rec = append(rec, inlineVal...)
		}
		if err := b.addRecord(rec); err != nil {
			// Roll back chains allocated during this failed encoding so the
			// free list does not leak pages.
			if keyOV != 0 {
				pg.Free(keyOV)
			}
			if valOV != 0 {
				pg.Free(valOV)
			}
			return nil, err
		}
		prevSuffix = suffix
	}
	return b.finish(uint16(len(entries)))
}

// leafDecode fully decodes a leaf page into ordered entries. Overflow chains
// are followed, so this is used for mutations, compaction and verification;
// hot-path searches use leafSearch instead and touch overflow pages only when
// the inline keys tie.
func leafDecode(pg Pager, buf []byte) (cp []byte, entries []LeafEntry, prev, next int) {
	n := int(u16(buf, offNKeys))
	rstart := int(u16(buf, offRStart))
	cpLen := int(u16(buf, offCPLen))
	cp = append([]byte(nil), buf[headerSize:headerSize+cpLen]...)
	prev = int(u64(buf, offPrev))
	next = int(u64(buf, offLink))
	entries = make([]LeafEntry, 0, n)

	var prevSuffix []byte
	for i := 0; i < n; i++ {
		off := int(u16(buf, rstart+i*slotSize))
		ln := int(u16(buf, rstart+i*slotSize+2))
		rec := buf[off : off+ln]
		p := 0
		flags := rec[p]
		p++

		var suffix []byte
		keyOV := 0
		if flags&flagRestart != 0 {
			v, k := binary.Uvarint(rec[p:])
			p += k
			slen := int(v)
			suffix = append([]byte(nil), rec[p:p+slen]...)
			p += slen
		} else {
			sv, k1 := binary.Uvarint(rec[p:])
			p += k1
			rv, k2 := binary.Uvarint(rec[p:])
			p += k2
			suffix = make([]byte, 0, int(sv)+int(rv))
			suffix = append(suffix, prevSuffix[:sv]...)
			suffix = append(suffix, rec[p:p+int(rv)]...)
			p += int(rv)
		}
		if flags&flagKeyOverflow != 0 {
			v, k := binary.Uvarint(rec[p:])
			p += k
			keyOV = int(v)
		}

		var val []byte
		valOV := 0
		if flags&flagValOverflow != 0 {
			v, k := binary.Uvarint(rec[p:])
			p += k
			valOV = int(v)
			val = readChain(pg, valOV)
		} else {
			v, k := binary.Uvarint(rec[p:])
			p += k
			vlen := int(v)
			val = append([]byte(nil), rec[p:p+vlen]...)
		}

		var key []byte
		if keyOV != 0 {
			// The overflow chain holds the complete key.
			key = readChain(pg, keyOV)
		} else {
			key = make([]byte, 0, cpLen+len(suffix))
			key = append(key, cp...)
			key = append(key, suffix...)
		}
		entries = append(entries, LeafEntry{Key: key, Value: val, KeyOV: keyOV, ValOV: valOV})
		prevSuffix = suffix
	}
	return cp, entries, prev, next
}

// leafSearch locates key inside a leaf page. It returns the slot index at
// which key is found or should be inserted, plus a found flag.
//
// Search strategy (requirements 6/7): binary search over restart slots only,
// then decode at most restartInterval records sequentially; every comparison
// first checks the page common prefix, so work starts at the first byte that
// could differ. Overflow chains are read only when inline prefixes tie.
func leafSearch(pg Pager, buf, key []byte) (slot int, found bool, ov int) {
	n := int(u16(buf, offNKeys))
	if n == 0 {
		return 0, false, 0
	}
	cpLen := int(u16(buf, offCPLen))
	cp := buf[headerSize : headerSize+cpLen]

	// Compare key against the page prefix.
	//   - key diverges below cp: every page key is greater -> insert at 0;
	//   - key diverges above cp: every page key is smaller -> insert at n;
	//   - key is a strict prefix of cp: every page key is greater, but the
	//     correct insertion position is 0 (the key sorts before the whole
	//     page), not n;
	//   - key starts with cp: fall through to per-record suffix comparison.
	if c := compareKeyPrefix(cp, key); c < 0 {
		return 0, false, 0
	} else if c > 0 {
		if len(key) < len(cp) {
			return 0, false, 0 // strict-prefix case: key belongs before the page
		}
		return n, false, 0
	}
	targetSuffix := key[cpLen:]
	rstart := int(u16(buf, offRStart))

	// restartSuffix decodes the stored suffix of slot i, which must be a
	// restart point (or an overflow record).
	restartSuffix := func(i int) (suffix []byte, flags byte) {
		off := int(u16(buf, rstart+i*slotSize))
		ln := int(u16(buf, rstart+i*slotSize+2))
		rec := buf[off : off+ln]
		flags = rec[0]
		v, k := binary.Uvarint(rec[1:])
		return rec[1+k : 1+k+int(v)], flags
	}

	// Binary search over restart slots: find the last restart whose stored key
	// is <= key. For overflow records the stored key is a truncated prefix, so
	// a strict-prefix tie counts as "before" and gets resolved linearly.
	lo, hi := 0, (n-1)/restartInterval
	base := 0
	for lo <= hi {
		mid := (lo + hi) / 2
		slotIdx := mid * restartInterval
		suf, flags := restartSuffix(slotIdx)
		if flags&flagRestart == 0 {
			panic(ErrCorrupt) // binary-search slots must be restart points
		}
		c := compareBytes(suf, targetSuffix)
		if flags&flagKeyOverflow != 0 && c == 0 && len(targetSuffix) > len(suf) {
			c = -1 // truncated inline key is a prefix of the target
		}
		if c <= 0 {
			base = slotIdx
			lo = mid + 1
		} else {
			if mid == 0 {
				return 0, false, 0
			}
			hi = mid - 1
		}
	}

	// Linear decode from `base`.
	var prevSuffix []byte
	for i := base; i < n; i++ {
		off := int(u16(buf, rstart+i*slotSize))
		ln := int(u16(buf, rstart+i*slotSize+2))
		rec := buf[off : off+ln]
		p := 1
		flags := rec[0]
		var suffix []byte
		keyOV := 0
		if flags&flagRestart != 0 {
			v, k := binary.Uvarint(rec[p:])
			p += k
			suffix = rec[p : p+int(v)]
			p += int(v)
		} else {
			sv, k1 := binary.Uvarint(rec[p:])
			p += k1
			rv, k2 := binary.Uvarint(rec[p:])
			p += k2
			suffix = append(append(make([]byte, 0, int(sv)+int(rv)), prevSuffix[:sv]...), rec[p:p+int(rv)]...)
			p += int(rv)
		}
		if flags&flagKeyOverflow != 0 {
			v, k := binary.Uvarint(rec[p:])
			p += k
			keyOV = int(v)
		}
		if keyOV != 0 {
			// Overflow record: the stored suffix is only a truncated prefix
			// of the real key, so suffix ordering cannot disambiguate. Read
			// the FULL key from the chain (requirement 5) and decide on it.
			// Multiple entries may share an inline prefix, so a full key below
			// the target means the answer is a later slot — keep scanning.
			full := readChain(pg, keyOV)
			switch compareBytes(full, key) {
			case 0:
				return i, true, keyOV
			case 1:
				return i, false, 0
			default: // full < key: continue to the next slot
				prevSuffix = suffix
				continue
			}
		}
		c := compareBytes(suffix, targetSuffix)
		if c == 0 {
			return i, true, 0
		} else if c > 0 {
			return i, false, 0
		}
		prevSuffix = suffix
	}
	return n, false, 0
}

// leafValueAt extracts just the value of the record at logical slot, without
// materializing every entry on the page. leafSearch must already have
// established that the slot holds the searched key.
func leafValueAt(pg Pager, buf []byte, slot int) []byte {
	n := int(u16(buf, offNKeys))
	if slot < 0 || slot >= n {
		panic(ErrCorrupt)
	}
	rstart := int(u16(buf, offRStart))
	off := int(u16(buf, rstart+slot*slotSize))
	ln := int(u16(buf, rstart+slot*slotSize+2))
	rec := buf[off : off+ln]
	p := 1 // skip flags
	flags := rec[0]
	if flags&flagRestart != 0 {
		slen, k := binary.Uvarint(rec[p:])
		p += k + int(slen)
	} else {
		_, k1 := binary.Uvarint(rec[p:])
		p += k1
		rv, k2 := binary.Uvarint(rec[p:])
		p += k2 + int(rv)
	}
	if flags&flagKeyOverflow != 0 {
		_, k := binary.Uvarint(rec[p:])
		p += k
	}
	if flags&flagValOverflow != 0 {
		ov, k := binary.Uvarint(rec[p:])
		p += k
		_ = p
		return readChain(pg, int(ov))
	}
	vlen, k := binary.Uvarint(rec[p:])
	p += k
	return append([]byte(nil), rec[p:p+int(vlen)]...)
}

// compareKeyPrefix compares the page common prefix against key:
// -1 if some key on the page sorts before key, +1 if all sort after,
// 0 if key starts with the prefix (per-record comparison still required).
func compareKeyPrefix(cp, key []byte) int {
	n := len(cp)
	if len(key) < n {
		n = len(key)
	}
	for i := 0; i < n; i++ {
		if cp[i] != key[i] {
			if cp[i] < key[i] {
				return -1
			}
			return 1
		}
	}
	if len(key) < len(cp) {
		// key is a strict prefix of cp: every page key is greater.
		return 1
	}
	return 0
}
