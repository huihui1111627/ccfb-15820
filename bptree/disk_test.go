package bptree

import (
	"math/rand"
	"path/filepath"
	"testing"
)

// TestDiskOverflowChains forces over-long keys through the FilePager so the
// overflow-chain write, eviction, reopen and free-list recycling paths all
// run against real disk pages.
func TestDiskOverflowChains(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overflow.db")
	rng := rand.New(rand.NewSource(11))
	const n = 200
	keys := make([][]byte, n)
	for i := range keys {
		keys[i] = genPartKey(rng, i, true) // ~430 bytes, page is 512 -> overflow
	}
	ref := &refModel{}

	func() {
		pg, err := NewFilePager(path, 512, 32)
		if err != nil {
			t.Fatal(err)
		}
		tr := Open(pg)
		for _, i := range rng.Perm(n) {
			v := []byte(randValue(rng))
			if err := tr.Put(keys[i], v); err != nil {
				t.Fatal(err)
			}
			ref.put(keys[i], v)
		}
		if st := tr.Stats(); st.OverflowPages == 0 {
			t.Fatal("expected overflow pages on disk")
		}
		if err := tr.CheckInvariants(); err != nil {
			t.Fatal(err)
		}
		if err := tr.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	pg, err := NewFilePager(path, 512, 32)
	if err != nil {
		t.Fatal(err)
	}
	tr := Open(pg)
	verifyAgainst(t, tr, ref)
	if err := tr.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
	// Delete every other key: overflow chains must be freed and the pages
	// recycled without corrupting surviving lookups.
	for _, i := range rng.Perm(n)[:n/2] {
		if err := tr.Delete(keys[i]); err != nil {
			t.Fatal(err)
		}
		ref.delete(keys[i])
	}
	verifyAgainst(t, tr, ref)
	if err := tr.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestCompactionReusesSpace checks requirement 4 behaviorally: after deleting
// most of a leaf's records and reinserting, the tree must stay correct and the
// leaf page count must not grow unbounded from dead space.
func TestCompactionReusesSpace(t *testing.T) {
	pg := NewMemoryPager(1024)
	tr := Open(pg)
	const n = 2000
	for i := 0; i < n; i++ {
		if err := tr.Put([]byte("PREFIX"+fmtSerial(i)), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	leavesAfterLoad := tr.LeafPages()
	// Delete three quarters, then reinsert the same keys.
	for i := 0; i < n; i++ {
		if i%4 != 0 {
			if err := tr.Delete([]byte("PREFIX" + fmtSerial(i))); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := 0; i < n; i++ {
		if i%4 != 0 {
			if err := tr.Put([]byte("PREFIX"+fmtSerial(i)), []byte("v")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tr.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
	if tr.Len() != n {
		t.Fatalf("Len after reinsert = %d, want %d", tr.Len(), n)
	}
	// Reinsertion reuses reclaimed space; leaf count should be close to the
	// post-load count, not many multiples of it.
	if got := tr.LeafPages(); got > leavesAfterLoad*2 {
		t.Fatalf("leaf pages grew to %d after reinsert (load had %d); space not reclaimed",
			got, leavesAfterLoad)
	}
}
