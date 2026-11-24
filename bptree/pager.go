package bptree

import (
	"fmt"
	"io"
	"os"
	"sync"
)

// Pager is the storage layer underneath the tree. All methods operate on
// whole fixed-size pages and return copies: mutating a buffer handed out by
// Read never corrupts cached state.
type Pager interface {
	// PageSize returns the fixed page size in bytes.
	PageSize() int
	// Alloc returns a fresh, zero-initialized page id (recycled free pages
	// first, then a newly appended page).
	Alloc() int
	// Read fetches a page. id must have been returned by Alloc and not freed.
	Read(id int) []byte
	// Write stores a page copy in the pager (flushed later by Sync/Close).
	Write(id int, data []byte)
	// Free releases a page (and, for overflow pages, its whole chain).
	Free(id int)
	// Stats reports allocation counters.
	Stats() PagerStats
	// Sync persists dirty state.
	Sync() error
	// Close flushes and releases resources.
	Close() error
}

// PagerStats summarizes pager allocation.
type PagerStats struct {
	PageSize   int
	TotalPages int // high-water mark of allocated page ids (including meta)
	FreePages  int // pages currently on the free list
	UsedPages  int // TotalPages - FreePages
}

// ---------------------------------------------------------------------------
// MemoryPager: a RAM-backed pager used by tests; it implements the exact same
// interface as the disk pager so the tree code is storage agnostic.
// ---------------------------------------------------------------------------

// MemoryPager keeps every page in a map. Free pages form the same linked
// list as on disk, which also exercises the recycling paths in tests.
type MemoryPager struct {
	mu        sync.RWMutex
	pageSize  int
	pages     map[int][]byte
	freeHead  int
	freeCount int
	high      int
}

// NewMemoryPager creates a RAM pager. Page 0 is the meta page.
func NewMemoryPager(pageSize int) *MemoryPager {
	if pageSize < 128 {
		panic("bptree: page size too small")
	}
	p := &MemoryPager{pageSize: pageSize, pages: make(map[int][]byte), freeHead: 0, high: 1}
	p.pages[0] = make([]byte, pageSize)
	return p
}

func (p *MemoryPager) PageSize() int { return p.pageSize }

func (p *MemoryPager) Alloc() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.freeHead != 0 {
		id := p.freeHead
		p.freeHead = int(u64(p.pages[id], offOverflowNext))
		p.freeCount--
		p.pages[id] = make([]byte, p.pageSize)
		return id
	}
	id := p.high
	p.high++
	p.pages[id] = make([]byte, p.pageSize)
	return id
}

func (p *MemoryPager) Read(id int) []byte {
	p.mu.RLock()
	defer p.mu.RUnlock()
	b, ok := p.pages[id]
	if !ok {
		panic(fmt.Sprintf("bptree: read of invalid page %d", id))
	}
	out := make([]byte, p.pageSize)
	copy(out, b)
	return out
}

func (p *MemoryPager) Write(id int, data []byte) {
	if len(data) != p.pageSize {
		panic("bptree: Write with wrong page size")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	b := make([]byte, p.pageSize)
	copy(b, data)
	p.pages[id] = b
}

func (p *MemoryPager) Free(id int) {
	if id <= 0 {
		panic("bptree: cannot free meta page")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.pages[id]; !ok {
		panic(fmt.Sprintf("bptree: double free of page %d", id))
	}
	// Overflow pages chain through offOverflowNext; follow the whole chain
	// so callers only free the head of a chain.
	cur := id
	for cur != 0 {
		next := 0
		if p.pages[cur][offType] == pageOverflow {
			next = int(u64(p.pages[cur], offOverflowNext))
		}
		zero := make([]byte, p.pageSize)
		putU64(zero, offOverflowNext, uint64(p.freeHead))
		p.pages[cur] = zero
		p.freeHead = cur
		p.freeCount++
		cur = next
	}
}

func (p *MemoryPager) Stats() PagerStats {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return PagerStats{
		PageSize:   p.pageSize,
		TotalPages: p.high,
		FreePages:  p.freeCount,
		UsedPages:  p.high - p.freeCount,
	}
}

func (p *MemoryPager) Sync() error { return nil }

func (p *MemoryPager) Close() error { return nil }

// ---------------------------------------------------------------------------
// FilePager: disk-backed pager with a fixed-size LRU frame cache.
// ---------------------------------------------------------------------------

type cacheFrame struct {
	id     int
	data   []byte
	dirty  bool
	linked bool
	prev   *cacheFrame
	next   *cacheFrame
}

// FilePager stores pages in a single growable file:
//
//	page 0: meta (magic, root id, leaf count, free-list head, page size)
//	pages 1..N: tree / overflow pages; freed pages link through the free list.
type FilePager struct {
	mu        sync.Mutex
	f         *os.File
	pageSize  int
	cacheCap  int
	cache     map[int]*cacheFrame
	head      *cacheFrame // most recently used
	tail      *cacheFrame // least recently used
	freeHead  int
	freeCount int
	high      int // next never-used page id
	root      int
	closed    bool
}

const metaMagic = uint64(0x4250545245450001) // "BPTREE\x00\x01"

const (
	metaOffMagic     = 0
	metaOffRoot      = 8
	metaOffLeafCount = 16
	metaOffFreeHead  = 24
	metaOffFreeCount = 32
	metaOffPageSize  = 40
	metaOffHigh      = 48
	metaSize         = 64
)

// NewFilePager opens (creating if needed) a page file. cacheFrames bounds the
// in-memory page cache; pass 0 for a default of 256 frames.
func NewFilePager(path string, pageSize, cacheFrames int) (*FilePager, error) {
	if pageSize < 128 {
		return nil, fmt.Errorf("bptree: page size too small")
	}
	if cacheFrames <= 0 {
		cacheFrames = 256
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	p := &FilePager{
		f:        f,
		pageSize: pageSize,
		cacheCap: cacheFrames,
		cache:    make(map[int]*cacheFrame),
		freeHead: 0,
		high:     1,
	}
	if info.Size() == 0 {
		// Fresh file: write the meta page and leave root unset (0).
		meta := make([]byte, pageSize)
		putU64(meta, metaOffMagic, metaMagic)
		putU64(meta, metaOffRoot, 0)
		putU64(meta, metaOffLeafCount, 0)
		putU64(meta, metaOffFreeHead, 0)
		putU64(meta, metaOffFreeCount, 0)
		putU64(meta, metaOffPageSize, uint64(pageSize))
		putU64(meta, metaOffHigh, 1)
		if _, err := f.WriteAt(meta, 0); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, err
		}
		p.high = 1
		return p, nil
	}
	meta := make([]byte, pageSize)
	if _, err := io.ReadFull(f, meta); err != nil {
		f.Close()
		return nil, err
	}
	if u64(meta, metaOffMagic) != metaMagic {
		f.Close()
		return nil, fmt.Errorf("bptree: %s is not a bptree page file", path)
	}
	if int(u64(meta, metaOffPageSize)) != pageSize {
		f.Close()
		return nil, fmt.Errorf("bptree: page size mismatch: file uses %d, opened with %d",
			u64(meta, metaOffPageSize), pageSize)
	}
	p.root = int(u64(meta, metaOffRoot))
	p.freeHead = int(u64(meta, metaOffFreeHead))
	p.freeCount = int(u64(meta, metaOffFreeCount))
	p.high = int(u64(meta, metaOffHigh))
	return p, nil
}

func (p *FilePager) PageSize() int { return p.pageSize }

// rootID / setRootID let the tree persist its root pointer through the pager.
func (p *FilePager) rootID() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.root
}

func (p *FilePager) setRootID(id int) {
	p.mu.Lock()
	p.root = id
	p.mu.Unlock()
}

// touch moves a frame to the MRU (head) position. Caller holds p.mu.
func (p *FilePager) touch(fr *cacheFrame) {
	if p.head == fr && fr.linked {
		return
	}
	if fr.linked {
		p.detach(fr)
	}
	fr.prev = nil
	fr.next = p.head
	if p.head != nil {
		p.head.prev = fr
	}
	p.head = fr
	if p.tail == nil {
		p.tail = fr
	}
	fr.linked = true
}

func (p *FilePager) detach(fr *cacheFrame) {
	if fr.prev != nil {
		fr.prev.next = fr.next
	} else {
		p.head = fr.next
	}
	if fr.next != nil {
		fr.next.prev = fr.prev
	} else {
		p.tail = fr.prev
	}
	fr.prev, fr.next = nil, nil
	fr.linked = false
}

// evict flushes the LRU frame until the cache is below capacity.
// Caller holds p.mu.
func (p *FilePager) evictOne() error {
	fr := p.tail
	if fr == nil {
		return nil
	}
	p.detach(fr)
	delete(p.cache, fr.id)
	if fr.dirty {
		if err := p.writeDisk(fr.id, fr.data); err != nil {
			return err
		}
	}
	return nil
}

func (p *FilePager) writeDisk(id int, data []byte) error {
	off := int64(id) * int64(p.pageSize)
	_, err := p.f.WriteAt(data, off)
	return err
}

func (p *FilePager) readDisk(id int) ([]byte, error) {
	buf := make([]byte, p.pageSize)
	off := int64(id) * int64(p.pageSize)
	if _, err := p.f.ReadAt(buf, off); err != nil {
		return nil, err
	}
	return buf, nil
}

func (p *FilePager) Alloc() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.freeHead != 0 {
		id := p.freeHead
		// The freed page's first bytes hold the next free id.
		buf, err := p.readDisk(id)
		if err != nil {
			panic(err)
		}
		p.freeHead = int(u64(buf, offOverflowNext))
		p.freeCount--
		// A recycled id might still be cached (clean, freed contents).
		if fr, ok := p.cache[id]; ok {
			p.detach(fr)
			delete(p.cache, id)
		}
		zero := make([]byte, p.pageSize)
		fr := &cacheFrame{id: id, data: zero, dirty: true}
		p.cache[id] = fr
		p.touch(fr)
		return id
	}
	id := p.high
	p.high++
	zero := make([]byte, p.pageSize)
	fr := &cacheFrame{id: id, data: zero, dirty: true}
	if len(p.cache) >= p.cacheCap {
		if err := p.evictOne(); err != nil {
			panic(err)
		}
	}
	p.cache[id] = fr
	p.touch(fr)
	return id
}

func (p *FilePager) Read(id int) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if fr, ok := p.cache[id]; ok {
		p.touch(fr)
		out := make([]byte, p.pageSize)
		copy(out, fr.data)
		return out
	}
	buf, err := p.readDisk(id)
	if err != nil {
		panic(fmt.Sprintf("bptree: read page %d: %v", id, err))
	}
	if len(p.cache) >= p.cacheCap {
		if err := p.evictOne(); err != nil {
			panic(err)
		}
	}
	fr := &cacheFrame{id: id, data: buf}
	p.cache[id] = fr
	p.touch(fr)
	out := make([]byte, p.pageSize)
	copy(out, buf)
	return out
}

func (p *FilePager) Write(id int, data []byte) {
	if len(data) != p.pageSize {
		panic("bptree: Write with wrong page size")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if fr, ok := p.cache[id]; ok {
		copy(fr.data, data)
		fr.dirty = true
		p.touch(fr)
		return
	}
	if len(p.cache) >= p.cacheCap {
		if err := p.evictOne(); err != nil {
			panic(err)
		}
	}
	buf := make([]byte, p.pageSize)
	copy(buf, data)
	fr := &cacheFrame{id: id, data: buf, dirty: true}
	p.cache[id] = fr
	p.touch(fr)
}

func (p *FilePager) Free(id int) {
	if id <= 0 {
		panic("bptree: cannot free meta page")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Walk an overflow chain if this is one; regular pages free themselves only.
	cur := id
	for cur != 0 {
		var next int
		if fr, ok := p.cache[cur]; ok {
			if fr.data[offType] == pageOverflow {
				next = int(u64(fr.data, offOverflowNext))
			}
			p.detach(fr)
			delete(p.cache, cur)
		} else {
			buf, err := p.readDisk(cur)
			if err != nil {
				panic(err)
			}
			if buf[offType] == pageOverflow {
				next = int(u64(buf, offOverflowNext))
			}
		}
		zero := make([]byte, p.pageSize)
		putU64(zero, offOverflowNext, uint64(p.freeHead))
		if err := p.writeDisk(cur, zero); err != nil {
			panic(err)
		}
		p.freeHead = cur
		p.freeCount++
		cur = next
	}
}

func (p *FilePager) Stats() PagerStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PagerStats{
		PageSize:   p.pageSize,
		TotalPages: p.high,
		FreePages:  p.freeCount,
		UsedPages:  p.high - p.freeCount,
	}
}

// writeMeta persists the meta page. Caller holds p.mu or is under Sync's lock.
func (p *FilePager) writeMetaLocked() error {
	meta := make([]byte, p.pageSize)
	putU64(meta, metaOffMagic, metaMagic)
	putU64(meta, metaOffRoot, uint64(p.root))
	putU64(meta, metaOffFreeHead, uint64(p.freeHead))
	putU64(meta, metaOffFreeCount, uint64(p.freeCount))
	putU64(meta, metaOffPageSize, uint64(p.pageSize))
	putU64(meta, metaOffHigh, uint64(p.high))
	return p.writeDisk(0, meta)
}

// Root returns the persisted root page id (0 before the first insert).
func (p *FilePager) Root() int { return p.rootID() }

// SetRoot persists a new root pointer.
func (p *FilePager) SetRoot(id int) { p.setRootID(id) }

// Name returns the underlying file path.
func (p *FilePager) Name() string { return p.f.Name() }

func (p *FilePager) Sync() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Flush every dirty frame.
	for fr := p.tail; fr != nil; fr = fr.prev {
		if fr.dirty {
			if err := p.writeDisk(fr.id, fr.data); err != nil {
				return err
			}
			fr.dirty = false
		}
	}
	if err := p.writeMetaLocked(); err != nil {
		return err
	}
	return p.f.Sync()
}

func (p *FilePager) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	for fr := p.tail; fr != nil; fr = fr.prev {
		if fr.dirty {
			if err := p.writeDisk(fr.id, fr.data); err != nil {
				return err
			}
			fr.dirty = false
		}
	}
	if err := p.writeMetaLocked(); err != nil {
		return err
	}
	if err := p.f.Sync(); err != nil {
		return err
	}
	return p.f.Close()
}
