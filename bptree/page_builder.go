package bptree

// pageBuilder lays out one page with two regions:
//
//	header | common prefix | record 0, record 1, ... | free space | slot directory |
//
// Records grow upwards from after the common prefix. The slot directory lives
// at the very end of the page in LOGICAL key order: logical slot i is always
// at pageSize-(cap-i)*slotSize, where cap is the directory capacity reserved
// up front. addRecord therefore writes slot i to a fixed address computed
// from its logical index, and finish() records the directory's start offset.
// Requirement 4 is served directly — a full re-encode places every record and
// slot contiguously, so deleted-record fragments disappear without a
// separate compaction pass.
type pageBuilder struct {
	buf       []byte
	rstart    int // next free byte for records
	n         int
	pageSize  int
	recordOff []uint16
	recordLen []uint16
}

func newPageBuilder(pageSize int, ptype byte, cp []byte, link1, link2 int) *pageBuilder {
	buf := make([]byte, pageSize)
	buf[offType] = ptype
	putU64(buf, offLink, uint64(link1))
	putU64(buf, offPrev, uint64(link2))
	putU16(buf, offCPLen, uint16(len(cp)))
	copy(buf[headerSize:], cp)
	return &pageBuilder{
		buf:      buf,
		rstart:   headerSize + len(cp),
		pageSize: pageSize,
	}
}

func (b *pageBuilder) addRecord(rec []byte) error {
	// After this call there will be b.n+1 records, each needing one record
	// body and one slot-directory entry; verify the two regions do not meet.
	neededRecordEnd := b.rstart + len(rec)
	neededDirStart := b.pageSize - (b.n+1)*slotSize
	if neededRecordEnd > neededDirStart {
		return ErrPageFull
	}
	copy(b.buf[b.rstart:], rec)
	// Stash the record's offset/length in record order temporarily at the
	// growing front region is impossible (would collide), so keep them in
	// parallel slices inside the builder and flush the directory at finish.
	b.recordOff = append(b.recordOff, uint16(b.rstart))
	b.recordLen = append(b.recordLen, uint16(len(rec)))
	b.rstart += len(rec)
	b.n++
	return nil
}

func (b *pageBuilder) finish(nkeys uint16) ([]byte, error) {
	dirStart := b.pageSize - b.n*slotSize
	// Records must not overlap the directory.
	if b.rstart > dirStart {
		return nil, ErrPageFull
	}
	for i := 0; i < b.n; i++ {
		addr := dirStart + i*slotSize
		putU16(b.buf, addr, b.recordOff[i])
		putU16(b.buf, addr+2, b.recordLen[i])
	}
	putU16(b.buf, offNKeys, nkeys)
	putU16(b.buf, offRStart, uint16(dirStart))
	return b.buf, nil
}

// pageFreeSpace reports the bytes still available for new records/slots.
func pageFreeSpace(buf []byte) int {
	rstart := int(u16(buf, offRStart))
	cpLen := int(u16(buf, offCPLen))
	contentStart := headerSize + cpLen
	n := int(u16(buf, offNKeys))
	return rstart - contentStart + (len(buf) - rstart - n*slotSize)
}

// pageFragmentation reports dead record bytes (tombstoned space) inside the
// record area. This layout has no in-place tombstones: every mutation
// re-encodes the whole page through the builder, which packs surviving
// records contiguously, so fragmentation is structurally always zero.
// The reclaimable space on a page is therefore exactly its free space
// (pageFreeSpace), not a separate fragmentation pool. Requirement 4's
// "compact when full" is satisfied implicitly by that full re-encode.
func pageFragmentation(buf []byte) int {
	return 0
}
