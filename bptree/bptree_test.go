package bptree

import (
	"bytes"
	"math/rand"
	"path/filepath"
	"testing"
)

// verifyAgainst compares every point lookup and a full ordered scan of the
// tree against the sorted-array oracle.
func verifyAgainst(t *testing.T, tr *Tree, ref *refModel) {
	t.Helper()
	if tr.Len() != len(ref.keys) {
		t.Fatalf("Len mismatch: tree=%d ref=%d", tr.Len(), len(ref.keys))
	}
	// Full ordered scan must equal the reference array key by key.
	got := tr.Scan(ScanOptions{})
	if len(got) != len(ref.keys) {
		t.Fatalf("scan length %d != ref %d", len(got), len(ref.keys))
	}
	for i := range got {
		if compareBytes(got[i].Key, ref.keys[i]) != 0 {
			t.Fatalf("scan key %d: %q != %q", i, got[i].Key, ref.keys[i])
		}
		if compareBytes(got[i].Value, ref.values[i]) != 0 {
			t.Fatalf("scan value for %q mismatch", got[i].Key)
		}
	}
	// Point lookups for every reference key.
	for i, k := range ref.keys {
		v, err := tr.Get(k)
		if err != nil {
			t.Fatalf("Get(%q): %v", k, err)
		}
		if compareBytes(v, ref.values[i]) != 0 {
			t.Fatalf("Get(%q) value mismatch", k)
		}
	}
}

func TestRandomWorkload(t *testing.T) {
	const (
		seed      = 42
		phases    = 6
		phaseOps  = 4000
		keyDomain = 6000 // forces lots of replacements and deletes
	)
	rng := rand.New(rand.NewSource(seed))
	tr := Open(NewMemoryPager(DefaultPageSize))
	defer tr.Close()
	ref := &refModel{}
	allKeys := make([][]byte, keyDomain)
	for i := range allKeys {
		allKeys[i] = genPartKey(rng, i, false)
	}

	for phase := 0; phase < phases; phase++ {
		for op := 0; op < phaseOps; op++ {
			k := allKeys[rng.Intn(keyDomain)]
			switch rng.Intn(10) {
			case 0, 1, 2, 3, 4, 5: // 60% inserts/replaces
				v := []byte(randValue(rng))
				if err := tr.Put(k, v); err != nil {
					t.Fatalf("put: %v", err)
				}
				ref.put(k, v)
			case 6, 7: // 20% deletes
				err := tr.Delete(k)
				ok := ref.delete(k)
				if ok && err != nil {
					t.Fatalf("delete existing key: %v", err)
				}
				if !ok && err != ErrNotFound {
					t.Fatalf("delete missing key returned %v want ErrNotFound", err)
				}
			case 8: // 10% point lookups
				v, err := tr.Get(k)
				rv, ok := ref.get(k)
				if ok {
					if err != nil || compareBytes(v, rv) != 0 {
						t.Fatalf("get mismatch for %q: err=%v", k, err)
					}
				} else if err != ErrNotFound {
					t.Fatalf("get missing %q returned %v", k, err)
				}
			default: // 10% scans
				lo := allKeys[rng.Intn(keyDomain)]
				hi := allKeys[rng.Intn(keyDomain)]
				if compareBytes(lo, hi) > 0 {
					lo, hi = hi, lo
				}
				got := tr.Scan(ScanOptions{Lower: lo, Upper: hi})
				want := ref.scan(lo, hi)
				if len(got) != len(want) {
					t.Fatalf("range scan len %d != %d", len(got), len(want))
				}
				for j := range got {
					if compareBytes(got[j].Key, want[j].Key) != 0 {
						t.Fatalf("range scan key mismatch at %d", j)
					}
				}
				// Prefix scan on the brand prefix.
				pref := lo[:6]
				upper := prefixSuccessor(pref)
				pgot := tr.PrefixScan(pref)
				pwant := ref.scan(pref, upper)
				if len(pgot) != len(pwant) {
					gs := map[string]bool{}
					for _, g := range pgot {
						gs[string(g.Key)] = true
					}
					for _, w := range pwant {
						if !gs[string(w.Key)] {
							t.Logf("MISSING %q treeLen=%d", w.Key, tr.Len())
						}
					}
					t.Fatalf("prefix scan len %d != %d (prefix=%q)", len(pgot), len(pwant), pref)
				}
				for j := range pgot {
					if !bytes.HasPrefix(pgot[j].Key, pref) {
						t.Fatalf("prefix scan returned key without prefix: %q", pgot[j].Key)
					}
					if compareBytes(pgot[j].Key, pwant[j].Key) != 0 {
						t.Fatalf("prefix scan key mismatch at %d", j)
					}
				}
			}
		}
		verifyAgainst(t, tr, ref)
		if err := tr.CheckInvariants(); err != nil {
			t.Fatalf("phase %d: %v", phase, err)
		}
		st := tr.Stats()
		if st.LeafPages != tr.LeafPages() {
			t.Fatalf("leaf page counter drift: %d != %d", st.LeafPages, tr.LeafPages())
		}
		t.Logf("phase %d: keys=%d height=%d leaves=%d interiors=%d avgFanout=%.1f avgPrefix=%.1f onDisk=%d logical=%d ratio=%.3f",
			phase, len(ref.keys), st.Height, st.LeafPages, st.InteriorPages,
			st.Levels[0].AvgFanout, st.Levels[0].AvgPrefix, st.OnDiskBytes, st.LogicalBytes, st.CompressionRate)
	}
	// Splits must all have produced valid shortest separators; invariants
	// already proved it, but assert the inspector recorded events.
	if len(tr.Splits()) == 0 {
		t.Fatal("expected split events to be recorded")
	}
	for _, ev := range tr.Splits() {
		if ev.SeparatorLen == 0 {
			t.Fatal("recorded split with empty separator")
		}
	}
}

func randValue(rng *rand.Rand) string {
	b := make([]byte, 8+rng.Intn(24))
	for i := range b {
		b[i] = byte('A' + rng.Intn(26))
	}
	return string(b)
}

// TestSmallPageOverflow forces keys past pageSize/4 so requirement 5's
// overflow chains, truncated inline comparisons and overflow ties all run.
func TestSmallPageOverflow(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	pageSize := 512
	tr := Open(NewMemoryPager(pageSize))
	defer tr.Close()
	ref := &refModel{}
	const n = 400
	keys := make([][]byte, n)
	for i := 0; i < n; i++ {
		keys[i] = genPartKey(rng, i, true) // ~430 bytes each
	}
	// Shuffled insertion.
	order := rng.Perm(n)
	for _, i := range order {
		v := []byte(randValue(rng))
		if err := tr.Put(keys[i], v); err != nil {
			t.Fatal(err)
		}
		ref.put(keys[i], v)
	}
	verifyAgainst(t, tr, ref)
	if err := tr.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
	st := tr.Stats()
	if st.OverflowPages == 0 {
		t.Fatal("expected overflow pages for long keys")
	}
	// Delete half and confirm chains are recycled.
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
	// Reinsert deleted keys: free-list recycling paths.
	for _, i := range rng.Perm(n)[:n/2] {
		v := []byte(randValue(rng))
		if err := tr.Put(keys[i], v); err != nil {
			t.Fatal(err)
		}
		ref.put(keys[i], v)
	}
	verifyAgainst(t, tr, ref)
	if err := tr.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}

// TestPrefixShrinkReencode makes a page's common prefix shrink and checks the
// full re-encode path (requirement 1).
func TestPrefixShrinkReencode(t *testing.T) {
	tr := Open(NewMemoryPager(1024))
	defer tr.Close()
	prefix := []byte("AAAABRAND20240101SN")
	for i := 0; i < 120; i++ {
		k := append(append([]byte(nil), prefix...), []byte(fmtSerial(i))...)
		if err := tr.Put(k, []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	// Insert a key sharing almost nothing with the prefix.
	odd := []byte("ZZZZOTHER99999999SN0000000000")
	if err := tr.Put(odd, []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := tr.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
	v, err := tr.Get(odd)
	if err != nil || string(v) != "v" {
		t.Fatalf("odd key get: %v %q", err, v)
	}
}

func fmtSerial(i int) string {
	const digits = "0123456789"
	out := make([]byte, 10)
	for j := 9; j >= 0; j-- {
		out[j] = digits[i%10]
		i /= 10
	}
	return string(out)
}

// TestCursorWalk validates cursor navigation across leaf boundaries.
func TestCursorWalk(t *testing.T) {
	tr := Open(NewMemoryPager(512))
	defer tr.Close()
	ref := &refModel{}
	rng := rand.New(rand.NewSource(99))
	const n = 500
	for i := 0; i < n; i++ {
		k := genPartKey(rng, i, false)
		v := []byte(randValue(rng))
		tr.Put(k, v)
		ref.put(k, v)
	}
	c := tr.Cursor()
	c.SeekFirst()
	i := 0
	for {
		if compareBytes(c.Key(), ref.keys[i]) != 0 {
			t.Fatalf("cursor key %d mismatch: %q vs %q", i, c.Key(), ref.keys[i])
		}
		i++
		if !c.Next() {
			break
		}
	}
	if i != len(ref.keys) {
		t.Fatalf("cursor walked %d keys, ref has %d", i, len(ref.keys))
	}
	// Seek to a middle key.
	mid := ref.keys[n/2]
	c.Seek(mid)
	if compareBytes(c.Key(), mid) != 0 {
		t.Fatalf("seek landed on %q want %q", c.Key(), mid)
	}
}

// TestFilePersistence runs a workload through the disk pager, closes,
// reopens and verifies every key and the root pointer survive.
func TestFilePersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tree.db")
	rng := rand.New(rand.NewSource(123))
	ref := &refModel{}
	const n = 1500
	// Pre-generate keys so the reopen/delete phase references exactly the
	// same brand/model randomness as the insert phase.
	keys := make([][]byte, n)
	for i := range keys {
		keys[i] = genPartKey(rng, i, false)
	}

	func() {
		pg, err := NewFilePager(path, 1024, 64)
		if err != nil {
			t.Fatal(err)
		}
		tr := Open(pg)
		for i := 0; i < n; i++ {
			v := []byte(randValue(rng))
			if err := tr.Put(keys[i], v); err != nil {
				t.Fatal(err)
			}
			ref.put(keys[i], v)
		}
		if err := tr.CheckInvariants(); err != nil {
			t.Fatal(err)
		}
		if err := tr.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	pg, err := NewFilePager(path, 1024, 64)
	if err != nil {
		t.Fatal(err)
	}
	tr := Open(pg)
	if tr.Root() == 0 {
		t.Fatal("root did not persist")
	}
	verifyAgainst(t, tr, ref)
	if err := tr.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
	// Mutate after reopen to exercise recycled ids on disk.
	for i := 0; i < 200; i++ {
		k := keys[i]
		if err := tr.Delete(k); err != nil {
			t.Fatal(err)
		}
		ref.delete(k)
	}
	verifyAgainst(t, tr, ref)
	if err := tr.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestMillionKeyStress is the headline scale test: a few million long-prefix
// part codes with a mixed workload, verified at checkpoints.
func TestMillionKeyStress(t *testing.T) {
	if testing.Short() {
		t.Skip("million-key stress test skipped in -short mode")
	}
	const (
		seed = 20261124
		n    = 1_000_000
	)
	rng := rand.New(rand.NewSource(seed))
	tr := Open(NewMemoryPager(DefaultPageSize))
	defer tr.Close()
	ref := &refModel{}
	keys := make([][]byte, n)
	for i := range keys {
		keys[i] = genPartKey(rng, i, false)
	}
	insertOrder := rng.Perm(n)
	checkpoint := n / 5
	for i, idx := range insertOrder {
		v := []byte(randValue(rng))
		if err := tr.Put(keys[idx], v); err != nil {
			t.Fatal(err)
		}
		ref.put(keys[idx], v)
		if (i+1)%checkpoint == 0 {
			if err := tr.CheckInvariants(); err != nil {
				t.Fatalf("checkpoint %d: %v", i+1, err)
			}
			st := tr.Stats()
			t.Logf("inserted %d: height=%d leaves=%d avgFanout=%.1f avgPrefix=%.1f ratio=%.3f",
				i+1, st.Height, st.LeafPages, st.Levels[0].AvgFanout,
				st.Levels[0].AvgPrefix, st.CompressionRate)
		}
	}
	verifyAgainst(t, tr, ref)

	// Mixed delete/get/scan phase on a 200k sample. Full reference scans are
	// O(range) on a million-element oracle, so they run only at checkpoints;
	// the hot loop stays dominated by tree point operations.
	sample := rng.Perm(n)[:200_000]
	for j, idx := range sample {
		switch j % 4 {
		case 0, 1:
			if err := tr.Delete(keys[idx]); err != nil {
				t.Fatal(err)
			}
			ref.delete(keys[idx])
		case 2:
			if _, err := tr.Get(keys[idx]); err != nil {
				t.Fatal(err)
			}
		default:
			if _, err := tr.Get(keys[idx]); err != nil && err != ErrNotFound {
				t.Fatal(err)
			}
		}
		if j%25_000 == 0 {
			if err := tr.CheckInvariants(); err != nil {
				t.Fatal(err)
			}
			// One bounded range scan cross-checked against the oracle.
			lo := keys[sample[j%len(sample)]]
			hi := keys[sample[(j+137)%len(sample)]]
			if compareBytes(lo, hi) > 0 {
				lo, hi = hi, lo
			}
			g := tr.Scan(ScanOptions{Lower: lo, Upper: hi, Limit: 64})
			w := ref.scan(lo, hi)
			if len(w) > 64 {
				w = w[:64]
			}
			if len(g) != len(w) {
				t.Fatalf("stress scan mismatch %d vs %d", len(g), len(w))
			}
		}
	}
	verifyAgainst(t, tr, ref)
	if err := tr.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
	st := tr.Stats()
	t.Logf("final: height=%d leaves=%d interiors=%d overflow=%d free=%d onDisk=%d logical=%d ratio=%.3f fill=%.3f",
		st.Height, st.LeafPages, st.InteriorPages, st.OverflowPages, st.FreePages,
		st.OnDiskBytes, st.LogicalBytes, st.CompressionRate, st.AvgLeafFill)
}
