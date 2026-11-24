package bptree

// Cursor walks the B+ tree in key order. Requirement 8 is handled here:
// range scans follow leaf sibling pointers instead of re-descending the
// tree, and each leaf decode reconstructs full keys from its page common
// prefix — so a changed prefix across a page boundary is picked up
// automatically with no caller-visible stitching.
type Cursor struct {
	t       *Tree
	pageID  int
	entries []LeafEntry
	slot    int
}

// Cursor returns a new cursor positioned before the first key.
func (t *Tree) Cursor() *Cursor {
	return &Cursor{t: t}
}

// Seek positions the cursor at the first key >= key.
func (c *Cursor) Seek(key []byte) {
	c.t.mu.RLock()
	defer c.t.mu.RUnlock()
	if c.t.root == 0 {
		c.pageID, c.slot, c.entries = 0, 0, nil
		return
	}
	pageID := c.t.root
	for {
		buf := c.t.pg.Read(pageID)
		if buf[offType] == pageLeaf {
			slot, _, _ := leafSearch(c.t.pg, buf, key)
			_, entries, _, _ := leafDecode(c.t.pg, buf)
			c.pageID = pageID
			c.entries = entries
			c.slot = slot
			return
		}
		pageID = interiorSearch(buf, key)
	}
}

// SeekFirst positions the cursor at the smallest key.
func (c *Cursor) SeekFirst() {
	c.t.mu.RLock()
	defer c.t.mu.RUnlock()
	if c.t.root == 0 {
		c.pageID, c.slot, c.entries = 0, 0, nil
		return
	}
	pageID := c.t.root
	for {
		buf := c.t.pg.Read(pageID)
		if buf[offType] == pageLeaf {
			_, entries, _, _ := leafDecode(c.t.pg, buf)
			c.pageID = pageID
			c.entries = entries
			c.slot = 0
			return
		}
		pageID = int(u64(buf, offLink))
	}
}

// Next advances the cursor, crossing leaf pages via the sibling pointer.
// It returns false when the walk is exhausted.
func (c *Cursor) Next() bool {
	c.t.mu.RLock()
	defer c.t.mu.RUnlock()
	if c.pageID == 0 {
		return false
	}
	c.slot++
	if c.slot < len(c.entries) {
		return true
	}
	buf := c.t.pg.Read(c.pageID)
	next := int(u64(buf, offLink))
	if next == 0 {
		c.pageID, c.entries, c.slot = 0, nil, 0
		return false
	}
	nbuf := c.t.pg.Read(next)
	_, entries, _, _ := leafDecode(c.t.pg, nbuf)
	c.pageID = next
	c.entries = entries
	c.slot = 0
	return len(entries) > 0
}

// Key returns the key at the cursor position.
func (c *Cursor) Key() []byte {
	if c.pageID == 0 || c.slot >= len(c.entries) {
		return nil
	}
	return c.entries[c.slot].Key
}

// Value returns the value at the cursor position.
func (c *Cursor) Value() []byte {
	if c.pageID == 0 || c.slot >= len(c.entries) {
		return nil
	}
	return c.entries[c.slot].Value
}

// ScanOptions bounds an open-ended or bounded range scan.
type ScanOptions struct {
	// Lower is inclusive; nil means start at the smallest key.
	Lower []byte
	// Upper is exclusive; nil means no upper bound.
	Upper []byte
	// Limit caps the number of returned pairs; 0 means unlimited.
	Limit int
}

// KeyValue is one scan result.
type KeyValue struct {
	Key   []byte
	Value []byte
}

// Scan returns all pairs in [Lower, Upper) in key order.
func (t *Tree) Scan(opts ScanOptions) []KeyValue {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []KeyValue
	if t.root == 0 {
		return out
	}
	// Descend to the leaf that contains Lower (or the leftmost leaf when
	// unbounded); the sibling chain then covers every subsequent leaf.
	pageID := t.root
	if opts.Lower != nil {
		for {
			buf := t.pg.Read(pageID)
			if buf[offType] == pageLeaf {
				break
			}
			pageID = interiorSearch(buf, opts.Lower)
		}
	} else {
		for {
			buf := t.pg.Read(pageID)
			if buf[offType] == pageLeaf {
				break
			}
			pageID = int(u64(buf, offLink))
		}
	}
	startSlot := 0
	if opts.Lower != nil {
		buf := t.pg.Read(pageID)
		startSlot, _, _ = leafSearch(t.pg, buf, opts.Lower)
	}
	count := 0
	for pageID != 0 {
		buf := t.pg.Read(pageID)
		_, entries, _, next := leafDecode(t.pg, buf)
		for i := startSlot; i < len(entries); i++ {
			if opts.Upper != nil && compareBytes(entries[i].Key, opts.Upper) >= 0 {
				return out
			}
			out = append(out, KeyValue{
				Key:   append([]byte(nil), entries[i].Key...),
				Value: append([]byte(nil), entries[i].Value...),
			})
			count++
			if opts.Limit > 0 && count >= opts.Limit {
				return out
			}
		}
		pageID = next
		startSlot = 0
	}
	return out
}

// PrefixScan returns every key having prefix, in key order. The upper bound
// is the successor of the prefix (prefixSuccessor); an all-0xFF prefix has no
// finite successor and is therefore scanned to the end.
func (t *Tree) PrefixScan(prefix []byte) []KeyValue {
	upper := prefixSuccessor(prefix)
	return t.Scan(ScanOptions{Lower: append([]byte(nil), prefix...), Upper: upper})
}
