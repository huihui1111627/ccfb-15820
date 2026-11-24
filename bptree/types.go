// Package bptree implements a disk-resident B+ tree index tuned for long,
// look-alike keys (such as watch-part codes that share a long brand/model
// prefix).
//
// Compression techniques implemented here:
//   - every page stores one common prefix shared by all keys on the page;
//     in-page records only carry the suffix;
//   - leaf records additionally use prefix-delta encoding against the
//     previous suffix, with full-suffix restart points at fixed intervals;
//   - interior separators are the shortest byte string that separates the
//     left and right subtrees, rather than a copy of a child key;
//   - splits are chosen inside a window around the middle by shortest
//     separator, not at a fixed midpoint;
//   - keys/values longer than a quarter page move to overflow chains and
//     only a truncated inline key remains on the page.
package bptree

import "errors"

// Page type tags, stored at offType.
const (
	pageLeaf     = 1
	pageInterior = 2
	pageOverflow = 3
)

// On-page header layout (all pages except the meta page, which has its own
// fixed layout in pager.go).
const (
	offType    = 0  // uint8  page type
	offNKeys   = 1  // uint16 number of records on the page
	offRStart  = 3  // uint16 offset where the slot directory starts
	offCPLen   = 5  // uint16 common-prefix length
	offLink    = 7  // uint64 leaf: next sibling; interior: first child
	offPrev    = 15 // uint64 leaf only: previous sibling
	headerSize = 23

	// Overflow pages: type byte at 0, next-chain pointer at offOverflowNext,
	// per-page payload length at offLenHint, payload starting at overflowHeader.
	offOverflowNext = 1
	offLenHint      = 9
	overflowHeader  = 16
	overflowNoNext  = 0

	// Slot directory entry: record offset (uint16) + record length (uint16).
	slotSize = 4

	// Every restartInterval-th leaf record stores a full suffix, so an
	// in-page search only ever decodes at most restartInterval records.
	restartInterval = 16

	// DefaultPageSize is the default page size in bytes.
	DefaultPageSize = 4096

	// Leaf record flag bits.
	flagKeyOverflow = 0x01 // key tail lives on an overflow chain
	flagValOverflow = 0x02 // value lives on an overflow chain
	flagRestart     = 0x80 // record carries a full suffix (restart point)
)

// Errors returned by the package.
var (
	ErrNotFound = errors.New("bptree: key not found")
	ErrPageFull = errors.New("bptree: page cannot fit another record")
	ErrCorrupt  = errors.New("bptree: corrupt page")
)

func putU16(b []byte, off int, v uint16) {
	b[off] = byte(v)
	b[off+1] = byte(v >> 8)
}

func u16(b []byte, off int) uint16 {
	return uint16(b[off]) | uint16(b[off+1])<<8
}

func putU64(b []byte, off int, v uint64) {
	for i := 0; i < 8; i++ {
		b[off+i] = byte(v >> (8 * i))
	}
}

func u64(b []byte, off int) uint64 {
	var v uint64
	for i := 0; i < 8; i++ {
		v |= uint64(b[off+i]) << (8 * i)
	}
	return v
}

// commonPrefixLen returns the length of the longest common prefix of a and b.
func commonPrefixLen(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

// commonPrefixAll returns the longest prefix shared by every key. The empty
// page and single-key pages return 0 / the full key respectively.
func commonPrefixAll(keys [][]byte) int {
	if len(keys) == 0 {
		return 0
	}
	p := len(keys[0])
	for _, k := range keys[1:] {
		p = commonPrefixLen(keys[0][:p], k)
		if p == 0 {
			return 0
		}
	}
	return p
}

// maxInlineLen is the threshold above which a key or value is pushed to an
// overflow chain: one quarter of the page (requirement 5).
func maxInlineLen(pageSize int) int { return pageSize / 4 }
