package bptree

// Len returns the number of distinct keys currently stored. It walks the
// leaves; callers in hot loops should count locally instead.
func (t *Tree) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.root == 0 {
		return 0
	}
	total := 0
	pageID := t.root
	// Descend to the leftmost leaf.
	for {
		buf := t.pg.Read(pageID)
		if buf[offType] == pageLeaf {
			break
		}
		pageID = int(u64(buf, offLink))
	}
	for pageID != 0 {
		buf := t.pg.Read(pageID)
		total += int(u16(buf, offNKeys))
		pageID = int(u64(buf, offLink))
	}
	return total
}

// LeafPages returns the number of leaf pages (maintained across splits).
func (t *Tree) LeafPages() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.leafCount
}

// Sync flushes the underlying pager and, for file pagers, persists the root
// and leaf counters.
func (t *Tree) Sync() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if fp, ok := t.pg.(*FilePager); ok {
		fp.SetRoot(t.root)
		meta := t.pg.Read(0)
		putU64(meta, metaOffRoot, uint64(t.root))
		putU64(meta, metaOffLeafCount, uint64(t.leafCount))
		t.pg.Write(0, meta)
	}
	return t.pg.Sync()
}

// Close syncs and closes the underlying pager.
func (t *Tree) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if fp, ok := t.pg.(*FilePager); ok {
		fp.SetRoot(t.root)
		meta := t.pg.Read(0)
		putU64(meta, metaOffRoot, uint64(t.root))
		putU64(meta, metaOffLeafCount, uint64(t.leafCount))
		t.pg.Write(0, meta)
	}
	return t.pg.Close()
}
