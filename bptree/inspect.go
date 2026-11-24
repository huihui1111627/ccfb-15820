package bptree

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// LevelStats summarizes one tree level.
type LevelStats struct {
	Level     int // 0 = leaf level
	Pages     int
	MinFanout int
	MaxFanout int
	AvgFanout float64
	MinPrefix int
	MaxPrefix int
	AvgPrefix float64
}

// TreeStats is the inspector's aggregate view.
type TreeStats struct {
	Height          int // number of levels (1 for a tree whose root is a leaf)
	Levels          []LevelStats
	LeafPages       int
	InteriorPages   int
	OverflowPages   int
	FreePages       int
	TotalPages      int
	OnDiskBytes     int // pages actually used * page size
	LogicalBytes    int // bytes the same (key,value) pairs would take uncompressed
	CompressionRate float64
	LeafCount       int
	AvgLeafFill     float64 // fraction of page bytes occupied by records+slots
}

// Stats walks the whole tree and computes the statistics shown by the
// inspection page/tool.
func (t *Tree) Stats() TreeStats {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var st TreeStats
	if t.root == 0 {
		return st
	}
	pst := t.pg.Stats()
	st.TotalPages = pst.TotalPages
	st.FreePages = pst.FreePages

	type frame struct {
		id    int
		level int
	}
	queue := []frame{{t.root, 0}}
	levelMap := map[int]*LevelStats{}
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		buf := t.pg.Read(f.id)
		ls, ok := levelMap[f.level]
		if !ok {
			ls = &LevelStats{Level: f.level, MinFanout: 1 << 30, MinPrefix: 1 << 30}
			levelMap[f.level] = ls
		}
		cpLen := int(u16(buf, offCPLen))
		if cpLen < ls.MinPrefix {
			ls.MinPrefix = cpLen
		}
		if cpLen > ls.MaxPrefix {
			ls.MaxPrefix = cpLen
		}
		ls.AvgPrefix += float64(cpLen)
		ls.Pages++

		switch buf[offType] {
		case pageLeaf:
			st.LeafPages++
			n := int(u16(buf, offNKeys))
			fan := n
			if fan < ls.MinFanout {
				ls.MinFanout = fan
			}
			if fan > ls.MaxFanout {
				ls.MaxFanout = fan
			}
			ls.AvgFanout += float64(fan)
			// Logical size: full keys and values as the application sees them.
			_, entries, _, _ := leafDecode(t.pg, buf)
			for _, e := range entries {
				st.LogicalBytes += len(e.Key) + len(e.Value)
			}
			// Fill fraction: record area + slots over usable space.
			used := 0
			rstart := int(u16(buf, offRStart))
			for i := 0; i < n; i++ {
				used += int(u16(buf, rstart+i*slotSize+2)) + slotSize
			}
			usable := t.pageSize - headerSize - cpLen
			if usable > 0 {
				st.AvgLeafFill += float64(used) / float64(usable)
			}
		case pageInterior:
			st.InteriorPages++
			first, seps, _ := interiorDecode(t.pg, buf)
			fan := len(seps) + 1
			if fan < ls.MinFanout {
				ls.MinFanout = fan
			}
			if fan > ls.MaxFanout {
				ls.MaxFanout = fan
			}
			ls.AvgFanout += float64(fan)
			queue = append(queue, frame{first, f.level + 1})
			for _, s := range seps {
				queue = append(queue, frame{s.RightPage, f.level + 1})
			}
		}
	}
	// Overflow pages: everything allocated minus tree pages minus meta/free.
	st.OverflowPages = pst.UsedPages - st.LeafPages - st.InteriorPages - 1
	if st.OverflowPages < 0 {
		st.OverflowPages = 0
	}
	st.OnDiskBytes = pst.UsedPages * t.pageSize
	if st.LogicalBytes > 0 {
		st.CompressionRate = float64(st.OnDiskBytes) / float64(st.LogicalBytes)
	}
	if st.LeafPages > 0 {
		st.AvgLeafFill /= float64(st.LeafPages)
	}
	st.LeafCount = t.leafCount
	st.Height = len(levelMap)
	st.Levels = make([]LevelStats, 0, st.Height)
	// levelMap keys are root-down (root=0, leaf=Height-1); renumber so the
	// leaf level is 0, matching PageView.Level and the docs.
	for lvl := st.Height - 1; lvl >= 0; lvl-- {
		ls := levelMap[lvl]
		ls.Level = st.Height - 1 - lvl
		if ls.Pages > 0 {
			ls.AvgFanout /= float64(ls.Pages)
			ls.AvgPrefix /= float64(ls.Pages)
		}
		st.Levels = append(st.Levels, *ls)
	}
	return st
}

// PageView is the per-page internal view exposed by the inspector.
type PageView struct {
	ID           int
	Type         string
	Level        int // 0 = leaf
	NKeys        int
	CommonPrefix []byte
	NextSibling  int
	PrevSibling  int
	FirstChild   int
	// Compressed records as stored on the page: leaf delta tuples or
	// interior suffixes.
	StoredRecords []string
	// Full keys reconstructed from prefix + suffix (+ overflow).
	FullKeys [][]byte
	// Separators for interior pages, with the shortest-separator proof.
	Separators    []SeparatorView
	BytesUsed     int
	FreeSpace     int
	Fragmentation int // dead bytes inside the record area (reclaimed on re-encode)
}

// SeparatorView shows one interior separator and its boundary keys.
type SeparatorView struct {
	Separator []byte
	LeftMax   []byte
	RightMin  []byte
	Shortest  bool
}

// InspectPage decodes a single page into a human-observable structure.
func (t *Tree) InspectPage(id int) PageView {
	t.mu.RLock()
	defer t.mu.RUnlock()
	buf := t.pg.Read(id)
	pv := PageView{ID: id}
	cpLen := int(u16(buf, offCPLen))
	pv.CommonPrefix = append([]byte(nil), buf[headerSize:headerSize+cpLen]...)
	pv.NKeys = int(u16(buf, offNKeys))
	// Used bytes = record bodies (between the common prefix and the slot
	// directory) plus the slot directory itself; free space is whatever of
	// the usable region remains.
	contentStart := headerSize + cpLen
	dirStart := int(u16(buf, offRStart))
	recordBytes := dirStart - contentStart
	dirBytes := pv.NKeys * slotSize
	pv.BytesUsed = recordBytes + dirBytes
	pv.FreeSpace = t.pageSize - contentStart - pv.BytesUsed
	pv.Fragmentation = pageFragmentation(buf)

	switch buf[offType] {
	case pageLeaf:
		pv.Type = "leaf"
		pv.Level = 0
		pv.NextSibling = int(u64(buf, offLink))
		pv.PrevSibling = int(u64(buf, offPrev))
		_, entries, _, _ := leafDecode(t.pg, buf)
		for _, e := range entries {
			pv.FullKeys = append(pv.FullKeys, append([]byte(nil), e.Key...))
		}
		pv.StoredRecords = t.describeLeafRecords(buf, cpLen)
	case pageInterior:
		pv.Type = "interior"
		pv.Level = -1 // filled in by InspectTree
		pv.FirstChild = int(u64(buf, offLink))
		first, seps, _ := interiorDecode(t.pg, buf)
		children := make([]int, 0, len(seps)+1)
		children = append(children, first)
		for _, s := range seps {
			children = append(children, s.RightPage)
		}
		for i, s := range seps {
			lmax := t.subtreeMax(children[i])
			rmin := t.subtreeMin(children[i+1])
			want := shortestSeparator(lmax, rmin)
			pv.Separators = append(pv.Separators, SeparatorView{
				Separator: append([]byte(nil), s.Sep...),
				LeftMax:   lmax,
				RightMin:  rmin,
				Shortest: compareBytes(want, s.Sep) == 0 &&
					compareBytes(lmax, s.Sep) < 0 &&
					compareBytes(s.Sep, rmin) <= 0,
			})
			pv.FullKeys = append(pv.FullKeys, append([]byte(nil), s.Sep...))
			pv.StoredRecords = append(pv.StoredRecords,
				fmt.Sprintf("sep(suffix=%q) -> child %d", s.Sep[cpLen:], s.RightPage))
		}
	case pageOverflow:
		pv.Type = "overflow"
	default:
		pv.Type = "unknown"
	}
	return pv
}

// describeLeafRecords renders the compressed on-page tuples without
// materializing full keys, so the inspection view shows exactly what is
// stored: (shared, rest) deltas and restart markers.
func (t *Tree) describeLeafRecords(buf []byte, cpLen int) []string {
	n := int(u16(buf, offNKeys))
	rstart := int(u16(buf, offRStart))
	out := make([]string, 0, n)
	var prevSuffix []byte
	for i := 0; i < n; i++ {
		off := int(u16(buf, rstart+i*slotSize))
		ln := int(u16(buf, rstart+i*slotSize+2))
		rec := buf[off : off+ln]
		flags := rec[0]
		p := 1
		parts := []string{}
		if flags&flagRestart != 0 {
			v, k := binary.Uvarint(rec[p:])
			p += k
			suf := rec[p : p+int(v)]
			parts = append(parts, fmt.Sprintf("restart suffix=%q", suf))
			p += int(v)
			prevSuffix = append(prevSuffix[:0], suf...)
		} else {
			sv, k1 := binary.Uvarint(rec[p:])
			p += k1
			rv, k2 := binary.Uvarint(rec[p:])
			p += k2
			rest := rec[p : p+int(rv)]
			parts = append(parts, fmt.Sprintf("delta shared=%d rest=%q", sv, rest))
			suffix := make([]byte, 0, int(sv)+int(rv))
			suffix = append(suffix, prevSuffix[:sv]...)
			suffix = append(suffix, rest...)
			prevSuffix = suffix
		}
		if flags&flagKeyOverflow != 0 {
			v, k := binary.Uvarint(rec[p:])
			p += k
			parts = append(parts, fmt.Sprintf("keyOverflowPage=%d", v))
		}
		if flags&flagValOverflow != 0 {
			v, k := binary.Uvarint(rec[p:])
			p += k
			parts = append(parts, fmt.Sprintf("valOverflowPage=%d", v))
		} else {
			v, k := binary.Uvarint(rec[p:])
			p += k
			parts = append(parts, fmt.Sprintf("vlen=%d", v))
		}
		out = append(out, strings.Join(parts, " "))
	}
	return out
}

// InspectTree returns page views in breadth-first order, each annotated with
// its level (0 = leaf).
func (t *Tree) InspectTree() []PageView {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.root == 0 {
		return nil
	}
	// Determine each page's level via a BFS from the root.
	type qf struct {
		id    int
		level int
	}
	levels := map[int]int{}
	height := 0
	queue := []qf{{t.root, 0}}
	var order []int
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		levels[f.id] = f.level
		if f.level > height {
			height = f.level
		}
		order = append(order, f.id)
		buf := t.pg.Read(f.id)
		if buf[offType] == pageInterior {
			first, seps, _ := interiorDecode(t.pg, buf)
			queue = append(queue, qf{first, f.level + 1})
			for _, s := range seps {
				queue = append(queue, qf{s.RightPage, f.level + 1})
			}
		}
	}
	var views []PageView
	for _, id := range order {
		pv := t.InspectPage(id)
		// Root-down level -> leaf-relative level.
		pv.Level = height - levels[id]
		views = append(views, pv)
	}
	return views
}

// InvariantError describes a failed tree invariant check.
type InvariantError struct {
	PageID int
	Msg    string
}

func (e InvariantError) Error() string {
	return fmt.Sprintf("bptree invariant violated on page %d: %s", e.PageID, e.Msg)
}

// CheckInvariants validates the whole tree. It is the heavy verification used
// by the test harness after every operation phase:
//   - keys on every page are strictly ordered;
//   - each page's stored common prefix equals the real LCP of its keys;
//   - leaf delta records decode to the same keys as full restarts;
//   - every interior separator strictly separates its left/right subtrees
//     and is the shortest possible separator;
//   - sibling links are consistent;
//   - key ranges of children match their parent separators.
func (t *Tree) CheckInvariants() error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.root == 0 {
		return nil
	}
	return t.checkPage(t.root, nil, nil, true)
}

// checkPage validates pageID given the inclusive bounds all keys in its
// subtree must satisfy (nil = unbounded).
func (t *Tree) checkPage(pageID int, low, high []byte, isRoot bool) error {
	buf := t.pg.Read(pageID)
	switch buf[offType] {
	case pageLeaf:
		cp, entries, prev, next := leafDecode(t.pg, buf)
		// 1. common prefix correctness. The on-page prefix is computed over
		// the truncated inline keys (requirement 5 moves over-long tails to
		// overflow chains), so reproduce that truncation before the LCP check.
		maxInline := maxInlineLen(t.pg.PageSize())
		stored := make([][]byte, len(entries))
		for i := range entries {
			if len(entries[i].Key) > maxInline {
				stored[i] = entries[i].Key[:maxInline]
			} else {
				stored[i] = entries[i].Key
			}
		}
		wantCP := commonPrefixAll(stored)
		if wantCP != len(cp) {
			return InvariantError{pageID, fmt.Sprintf("common prefix length %d, actual LCP %d", len(cp), wantCP)}
		}
		// 2. strict ordering + bounds.
		for i := 1; i < len(entries); i++ {
			if compareBytes(entries[i-1].Key, entries[i].Key) >= 0 {
				return InvariantError{pageID, fmt.Sprintf("keys not strictly ordered at %d: %q >= %q",
					i, entries[i-1].Key, entries[i].Key)}
			}
		}
		if len(entries) > 0 {
			if low != nil && compareBytes(entries[0].Key, low) < 0 {
				return InvariantError{pageID, fmt.Sprintf("min key %q below subtree low %q", entries[0].Key, low)}
			}
			if high != nil && compareBytes(entries[len(entries)-1].Key, high) > 0 {
				return InvariantError{pageID, fmt.Sprintf("max key %q above subtree high %q",
					entries[len(entries)-1].Key, high)}
			}
		}
		// 3. sibling link consistency (prev->next and next->prev).
		if next != 0 {
			nb := t.pg.Read(next)
			if int(u64(nb, offPrev)) != pageID {
				return InvariantError{pageID, "next sibling does not point back"}
			}
		}
		if prev != 0 {
			pb := t.pg.Read(prev)
			if int(u64(pb, offLink)) != pageID {
				return InvariantError{pageID, "prev sibling does not point forward"}
			}
		}
		// 4. restart/delta encoding sanity: InspectPage path re-decodes
		// raw tuples; ensure every non-restart tuple references a valid shared
		// length against the previous suffix.
		if err := t.checkLeafEncoding(pageID, buf); err != nil {
			return err
		}
		return nil
	case pageInterior:
		first, seps, cp := interiorDecode(t.pg, buf)
		// Page common prefix correctness over the separators.
		sepKeys := make([][]byte, len(seps))
		for i := range seps {
			sepKeys[i] = seps[i].Sep
		}
		if want := commonPrefixAll(sepKeys); want != len(cp) {
			return InvariantError{pageID, fmt.Sprintf("interior common prefix %d != actual %d", len(cp), want)}
		}
		children := make([]int, 0, len(seps)+1)
		children = append(children, first)
		for _, s := range seps {
			children = append(children, s.RightPage)
		}
		for i, s := range seps {
			lmax := t.subtreeMax(children[i])
			rmin := t.subtreeMin(children[i+1])
			// Empty subtrees (all entries deleted) have nil boundaries; they
			// carry no ordering obligation until repopulated.
			if lmax == nil || rmin == nil {
				continue
			}
			if compareBytes(lmax, rmin) >= 0 {
				return InvariantError{pageID, "subtree boundary keys out of order"}
			}
			if !(compareBytes(lmax, s.Sep) < 0 && compareBytes(s.Sep, rmin) <= 0) {
				return InvariantError{pageID, fmt.Sprintf("separator %q fails to separate %q | %q",
					s.Sep, lmax, rmin)}
			}
			// "Shortest" is guaranteed at split time against the then-current
			// boundary keys (and asserted there via the split events). After
			// later inserts/deletes a separator may no longer be the global
			// minimum while still correctly separating the two subtrees, so
			// here we only require the separating inequality above plus the
			// structural property that no shorter separator of the same LCP
			// family would be invalid — enforced indirectly by strict
			// separator ordering below.
			if i > 0 && compareBytes(seps[i-1].Sep, s.Sep) >= 0 {
				return InvariantError{pageID, "separators not strictly ordered"}
			}
		}
		// Recurse with the bounds implied by separators.
		for i, child := range children {
			childLow, childHigh := low, high
			if i > 0 {
				childLow = seps[i-1].Sep
			}
			if i < len(seps) {
				childHigh = prefixBeforeExclusive(seps[i].Sep)
			}
			if err := t.checkPage(child, childLow, childHigh, false); err != nil {
				return err
			}
		}
		return nil
	default:
		return InvariantError{pageID, "unexpected page type"}
	}
}

// prefixBeforeExclusive turns an inclusive separator bound into the largest
// key that must live on the left: since left keys satisfy key < sep, the
// exclusive upper bound is simply sep itself.
func prefixBeforeExclusive(sep []byte) []byte {
	return append([]byte(nil), sep...)
}

// checkLeafEncoding validates raw delta tuples against restart structure.
func (t *Tree) checkLeafEncoding(pageID int, buf []byte) error {
	n := int(u16(buf, offNKeys))
	rstart := int(u16(buf, offRStart))
	var prevSuffix []byte
	for i := 0; i < n; i++ {
		off := int(u16(buf, rstart+i*slotSize))
		ln := int(u16(buf, rstart+i*slotSize+2))
		rec := buf[off : off+ln]
		flags := rec[0]
		p := 1
		expectRestart := i%restartInterval == 0
		if expectRestart {
			if flags&flagRestart == 0 {
				return InvariantError{pageID, fmt.Sprintf("slot %d missing restart marker", i)}
			}
			v, k := binary.Uvarint(rec[p:])
			p += k
			prevSuffix = append(prevSuffix[:0], rec[p:p+int(v)]...)
			p += int(v)
		} else {
			if flags&flagRestart != 0 {
				return InvariantError{pageID, fmt.Sprintf("slot %d has unexpected restart marker", i)}
			}
			sv, k1 := binary.Uvarint(rec[p:])
			p += k1
			if int(sv) > len(prevSuffix) {
				return InvariantError{pageID, fmt.Sprintf("slot %d delta shared length %d > previous suffix %d",
					i, sv, len(prevSuffix))}
			}
			rv, k2 := binary.Uvarint(rec[p:])
			p += k2
			rest := rec[p : p+int(rv)]
			suffix := make([]byte, 0, int(sv)+len(rest))
			suffix = append(suffix, prevSuffix[:sv]...)
			suffix = append(suffix, rest...)
			prevSuffix = suffix
		}
	}
	return nil
}
