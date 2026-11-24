package bptree

import (
	"math/rand"
	"testing"
)

func BenchmarkPut(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	keys := make([][]byte, b.N)
	for i := range keys {
		keys[i] = genPartKey(rng, i, false)
	}
	tr := Open(NewMemoryPager(DefaultPageSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := tr.Put(keys[i], []byte("bench-value")); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGet(b *testing.B) {
	const n = 200_000
	rng := rand.New(rand.NewSource(2))
	keys := make([][]byte, n)
	for i := range keys {
		keys[i] = genPartKey(rng, i, false)
	}
	tr := Open(NewMemoryPager(DefaultPageSize))
	for i := 0; i < n; i++ {
		if err := tr.Put(keys[i], []byte("bench-value")); err != nil {
			b.Fatal(err)
		}
	}
	lookup := rng.Perm(n)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := tr.Get(keys[lookup[i%n]]); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPrefixScan(b *testing.B) {
	const n = 200_000
	rng := rand.New(rand.NewSource(3))
	tr := Open(NewMemoryPager(DefaultPageSize))
	for i := 0; i < n; i++ {
		if err := tr.Put(genPartKey(rng, i, false), []byte("v")); err != nil {
			b.Fatal(err)
		}
	}
	prefix := []byte("ROLEX ")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := tr.PrefixScan(prefix); len(got) == 0 {
			b.Fatal("empty prefix scan")
		}
	}
}
