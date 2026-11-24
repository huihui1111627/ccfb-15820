package bptree

import (
	"encoding/binary"
	"sort"
	"sync"
)

// maxSplitEvents bounds how many split events are retained for the inspector.
const maxSplitEvents = 4096

// Tree is a disk-resident B+ tree. It is safe for concurrent readers and a
// single writer; callers needing concurrent writers serialize externally.
type Tree struct {
	mu        sync.RWMutex
	pg        Pager
	pageSize  int
	root      int // 0 = empty tree
	leafCount int
	splits    []SplitEvent
}

// SplitEvent records one page split for the inspector (requirement: show the
// separator chosen at split time).
type SplitEvent struct {
	Level         int // 0 = leaf level
	ParentPage    int
	LeftPage      int
	RightPage     int
	Separator     []byte
	SeparatorLen  int
	ChosenAt      int // split index inside the candidate window
	WindowLow     int
	WindowHigh    int
	Candidates    int
	EntriesOnPage int
}

// Open opens a tree on an existing pager. For FilePager the root is read from
// the meta page; for MemoryPager the tree starts empty.
func Open(pg Pager) *Tree {
	t := &Tree{pg: pg, pageSize: pg.PageSize()}
	if fp, ok := pg.(*FilePager); ok {
		t.root = fp.Root()
		meta := pg.Read(0)
		t.leafCount = int(u64(meta, metaOffLeafCount))
	}
	return t
}

// Pager exposes the underlying pager (used by the inspector and tests).
func (t *Tree) Pager() Pager { return t.pg }

// Root returns the root page id, 0 for an empty tree.
func (t *Tree) Root() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.root
}

func (t *Tree) initRootLeaf() {
	buf, err := leafEncode(t.pg, nil, 0, 0)
	if err != nil {
		panic(err)
	}
	id := t.pg.Alloc()
	t.pg.Write(id, buf)
	t.root = id
	t.leafCount = 1
	if fp, ok := t.pg.(*FilePager); ok {
		fp.SetRoot(id)
	}
}

// ---------------------------------------------------------------------------
// Get
// ---------------------------------------------------------------------------

// Get returns the value for key, or ErrNotFound.
func (t *Tree) Get(key []byte) ([]byte, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.root == 0 {
		return nil, ErrNotFound
	}
	pageID := t.root
	for {
		buf := t.pg.Read(pageID)
		switch buf[offType] {
		case pageInterior:
			pageID = interiorSearch(buf, key)
		case pageLeaf:
			slot, found, _ := leafSearch(t.pg, buf, key)
			if !found {
				return nil, ErrNotFound
			}
			return leafValueAt(t.pg, buf, slot), nil
		default:
			panic(ErrCorrupt)
		}
	}
}

// ---------------------------------------------------------------------------
// Put
// ---------------------------------------------------------------------------

// Put inserts or replaces a key/value pair.
func (t *Tree) Put(key, value []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.root == 0 {
		t.initRootLeaf()
	}
	sep, right, _, err := t.insertRec(t.root, 0, key, value)
	if err != nil {
		return err
	}
	if right != 0 {
		// Root split: a new interior page with one separator.
		buf, err := interiorEncode(t.pg,
			[]InteriorEntry{{Sep: sep, RightPage: right}}, t.root)
		if err != nil {
			return err
		}
		id := t.pg.Alloc()
		t.pg.Write(id, buf)
		t.root = id
		if fp, ok := t.pg.(*FilePager); ok {
			fp.SetRoot(id)
		}
	}
	return nil
}

// insertRec inserts into the subtree rooted at pageID. When that page splits
// it returns (separator, newRightPageID, leafLevel) so the parent can absorb
// the new child; right == 0 means no split.
func (t *Tree) insertRec(pageID, level int, key, value []byte) (sep []byte, right int, splitLevel int, err error) {
	buf := t.pg.Read(pageID)
	switch buf[offType] {
	case pageLeaf:
		return t.insertLeaf(pageID, buf, key, value)
	case pageInterior:
		child := interiorSearch(buf, key)
		childSep, childRight, childLevel, ierr := t.insertRec(child, level+1, key, value)
		if ierr != nil {
			return nil, 0, 0, ierr
		}
		if childRight == 0 {
			return nil, 0, 0, nil
		}
		return t.insertIntoInterior(pageID, buf, child, childSep, childRight, childLevel)
	default:
		panic(ErrCorrupt)
	}
}

// insertLeaf inserts into one leaf page, splitting if necessary.
func (t *Tree) insertLeaf(pageID int, buf, key, value []byte) ([]byte, int, int, error) {
	_, entries, prev, next := leafDecode(t.pg, buf)
	pos := sort.Search(len(entries), func(i int) bool {
		return compareBytes(entries[i].Key, key) >= 0
	})
	if pos < len(entries) && compareBytes(entries[pos].Key, key) == 0 {
		// Replace: free any old overflow chains first.
		if entries[pos].KeyOV != 0 {
			t.pg.Free(entries[pos].KeyOV)
		}
		if entries[pos].ValOV != 0 {
			t.pg.Free(entries[pos].ValOV)
		}
		entries[pos] = LeafEntry{Key: append([]byte(nil), key...), Value: append([]byte(nil), value...)}
		encoded, err := leafEncode(t.pg, entries, prev, next)
		if err != nil {
			return nil, 0, 0, err
		}
		t.pg.Write(pageID, encoded)
		return nil, 0, 0, nil
	}
	ne := LeafEntry{Key: append([]byte(nil), key...), Value: append([]byte(nil), value...)}
	entries = append(entries, LeafEntry{})
	copy(entries[pos+1:], entries[pos:])
	entries[pos] = ne

	encoded, err := leafEncode(t.pg, entries, prev, next)
	if err == nil {
		t.pg.Write(pageID, encoded)
		return nil, 0, 0, nil
	}
	// Requirement 4: before giving up and splitting, re-encode once more —
	// leafEncode already compacts, so a failure here is a genuine overflow.
	return t.splitLeaf(pageID, entries, prev, next)
}

// splitWindow returns the candidate split-index window around the middle.
// Requirement 3 asks for the shortest separator among positions NEAR the
// middle rather than a fixed midpoint — "near" is deliberately tight so both
// halves stay balanced: the radius is 1/16 of the entries, clamped to
// [1, 4]. Every index in [lo, hi] is still a valid ordered cut of a sorted
// entry array, so ordering can never be violated by the choice of cut.
func splitWindow(n int) (lo, hi int) {
	mid := n / 2
	radius := n / 16
	if radius < 1 {
		radius = 1
	}
	if radius > 4 {
		radius = 4
	}
	lo = mid - radius
	if lo < 1 {
		lo = 1
	}
	hi = mid + radius
	if hi > n-1 {
		hi = n - 1
	}
	return lo, hi
}

// splitLeaf cuts a leaf into two. Requirement 3: instead of a fixed midpoint
// every split index in a window around the middle is scored on the length of
// the resulting shortest separator (shorter wins), with balance as the
// tie-breaker. Both halves must still encode successfully.
func (t *Tree) splitLeaf(pageID int, entries []LeafEntry, prev, next int) ([]byte, int, int, error) {
	n := len(entries)
	lo, hi := splitWindow(n)
	type candidate struct {
		idx     int
		sepLen  int
		balance int
	}
	var cands []candidate
	for i := lo; i <= hi; i++ {
		sep := shortestSeparator(entries[i-1].Key, entries[i].Key)
		bal := i - (n - i)
		if bal < 0 {
			bal = -bal
		}
		cands = append(cands, candidate{i, len(sep), bal})
	}
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].sepLen != cands[b].sepLen {
			return cands[a].sepLen < cands[b].sepLen
		}
		return cands[a].balance < cands[b].balance
	})

	for _, c := range cands {
		left := entries[:c.idx]
		rightEntries := entries[c.idx:]
		rightID := t.pg.Alloc()
		// Right leaf takes over the old page's next link and points back to
		// the old page (which becomes the left half).
		rightBuf, rerr := leafEncode(t.pg, rightEntries, pageID, next)
		if rerr != nil {
			continue
		}
		leftBuf, lerr := leafEncode(t.pg, left, prev, rightID)
		if lerr != nil {
			t.pg.Free(rightID)
			continue
		}
		t.pg.Write(rightID, rightBuf)
		t.pg.Write(pageID, leftBuf)
		// The old next sibling must point back at the new right leaf.
		if next != 0 {
			nb := t.pg.Read(next)
			putU64(nb, offPrev, uint64(rightID))
			t.pg.Write(next, nb)
		}
		t.leafCount++
		sep := shortestSeparator(left[len(left)-1].Key, rightEntries[0].Key)

		t.recordSplit(SplitEvent{
			Level:         0,
			ParentPage:    pageID,
			LeftPage:      pageID,
			RightPage:     rightID,
			Separator:     append([]byte(nil), sep...),
			SeparatorLen:  len(sep),
			ChosenAt:      c.idx,
			WindowLow:     lo,
			WindowHigh:    hi,
			Candidates:    len(cands),
			EntriesOnPage: n,
		})
		return sep, rightID, 0, nil
	}
	return nil, 0, 0, ErrPageFull
}

// insertIntoInterior absorbs a child split into an interior page, splitting
// the interior itself when it overflows.
func (t *Tree) insertIntoInterior(pageID int, buf []byte, oldChild int, sep []byte, newRight, childLevel int) ([]byte, int, int, error) {
	firstChild, seps, _ := interiorDecode(t.pg, buf)
	pos := sort.Search(len(seps), func(i int) bool {
		return compareBytes(seps[i].Sep, sep) >= 0
	})
	seps = append(seps, InteriorEntry{})
	copy(seps[pos+1:], seps[pos:])
	seps[pos] = InteriorEntry{Sep: append([]byte(nil), sep...), RightPage: newRight}

	encoded, err := interiorEncode(t.pg, seps, firstChild)
	if err == nil {
		t.pg.Write(pageID, encoded)
		return nil, 0, 0, nil
	}
	return t.splitInterior(pageID, firstChild, seps, childLevel)
}

// splitInterior splits an interior page. The promoted separator is recomputed
// from the real boundary keys of the two subtrees (requirement 2), and the
// split index is chosen by shortest separator inside the middle window
// (requirement 3).
func (t *Tree) splitInterior(pageID, firstChild int, seps []InteriorEntry, childLevel int) ([]byte, int, int, error) {
	n := len(seps)
	lo, hi := splitWindow(n + 1)

	// children lists the child page ids in order: firstChild then each
	// separator's RightPage.
	children := make([]int, 0, n+1)
	children = append(children, firstChild)
	for _, s := range seps {
		children = append(children, s.RightPage)
	}

	type candidate struct {
		idx     int // number of children in the left half
		sepLen  int
		balance int
	}
	var cands []candidate
	for i := lo; i <= hi; i++ {
		leftMax := t.subtreeMax(children[i-1])
		rightMin := t.subtreeMin(children[i])
		sep := shortestSeparator(leftMax, rightMin)
		bal := i - (n + 1 - i)
		if bal < 0 {
			bal = -bal
		}
		cands = append(cands, candidate{i, len(sep), bal})
	}
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].sepLen != cands[b].sepLen {
			return cands[a].sepLen < cands[b].sepLen
		}
		return cands[a].balance < cands[b].balance
	})

	for _, c := range cands {
		leftChildren := children[:c.idx]
		rightChildren := children[c.idx:]
		leftSeps := seps[:c.idx-1]
		rightSeps := seps[c.idx:]

		rightID := t.pg.Alloc()
		rightBuf, rerr := interiorEncode(t.pg, rightSeps, rightChildren[0])
		if rerr != nil {
			continue
		}
		leftBuf, lerr := interiorEncode(t.pg, leftSeps, leftChildren[0])
		if lerr != nil {
			t.pg.Free(rightID)
			continue
		}
		t.pg.Write(rightID, rightBuf)
		t.pg.Write(pageID, leftBuf)

		promoted := shortestSeparator(t.subtreeMax(leftChildren[len(leftChildren)-1]),
			t.subtreeMin(rightChildren[0]))
		t.recordSplit(SplitEvent{
			Level:         childLevel + 1,
			ParentPage:    pageID,
			LeftPage:      pageID,
			RightPage:     rightID,
			Separator:     append([]byte(nil), promoted...),
			SeparatorLen:  len(promoted),
			ChosenAt:      c.idx,
			WindowLow:     lo,
			WindowHigh:    hi,
			Candidates:    len(cands),
			EntriesOnPage: n + 1,
		})
		return promoted, rightID, childLevel + 1, nil
	}
	return nil, 0, 0, ErrPageFull
}

// subtreeMin / subtreeMax walk down to the leftmost/rightmost leaf of ONE
// subtree and return its boundary full key. They never cross leaf sibling
// links: a sibling belongs to the global leaf sequence, not necessarily to
// this subtree, so following it could return a key outside the subtree's
// separator range. An emptied leaf (all entries deleted) yields nil; the
// caller decides how to treat the empty subtree.
func (t *Tree) subtreeMin(pageID int) []byte {
	for {
		buf := t.pg.Read(pageID)
		if buf[offType] == pageLeaf {
			_, entries, _, _ := leafDecode(t.pg, buf)
			if len(entries) == 0 {
				return nil
			}
			return entries[0].Key
		}
		pageID = int(u64(buf, offLink))
	}
}

func (t *Tree) subtreeMax(pageID int) []byte {
	for {
		buf := t.pg.Read(pageID)
		if buf[offType] == pageLeaf {
			_, entries, _, _ := leafDecode(t.pg, buf)
			if len(entries) == 0 {
				return nil
			}
			return entries[len(entries)-1].Key
		}
		n := int(u16(buf, offNKeys))
		if n == 0 {
			pageID = int(u64(buf, offLink))
			continue
		}
		rstart := int(u16(buf, offRStart))
		off := int(u16(buf, rstart+(n-1)*slotSize))
		ln := int(u16(buf, rstart+(n-1)*slotSize+2))
		rec := buf[off : off+ln]
		// Last varint in the record is the child pointer.
		var last uint64
		p := 0
		for p < ln {
			v, k := binary.Uvarint(rec[p:])
			p += k
			last = v
		}
		pageID = int(last)
	}
}

func (t *Tree) recordSplit(ev SplitEvent) {
	if len(t.splits) >= maxSplitEvents {
		// Drop the oldest half so a long run still leaves a recent sample.
		t.splits = append(t.splits[:0], t.splits[len(t.splits)/2:]...)
	}
	t.splits = append(t.splits, ev)
}

// Splits returns a copy of retained split events.
func (t *Tree) Splits() []SplitEvent {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]SplitEvent, len(t.splits))
	copy(out, t.splits)
	return out
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

// Delete removes key. It returns ErrNotFound when the key is absent. Deleted
// space is reclaimed by the next full re-encode of the page; an over-full
// page is compacted before any split is attempted (see insertLeaf).
func (t *Tree) Delete(key []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.root == 0 {
		return ErrNotFound
	}
	pageID := t.root
	var path []int
	for {
		buf := t.pg.Read(pageID)
		if buf[offType] == pageLeaf {
			_, entries, prev, next := leafDecode(t.pg, buf)
			pos := sort.Search(len(entries), func(i int) bool {
				return compareBytes(entries[i].Key, key) >= 0
			})
			if pos >= len(entries) || compareBytes(entries[pos].Key, key) != 0 {
				return ErrNotFound
			}
			if entries[pos].KeyOV != 0 {
				t.pg.Free(entries[pos].KeyOV)
			}
			if entries[pos].ValOV != 0 {
				t.pg.Free(entries[pos].ValOV)
			}
			entries = append(entries[:pos], entries[pos+1:]...)
			encoded, err := leafEncode(t.pg, entries, prev, next)
			if err != nil {
				return err
			}
			t.pg.Write(pageID, encoded)
			return nil
		}
		path = append(path, pageID)
		pageID = interiorSearch(buf, key)
	}
}
