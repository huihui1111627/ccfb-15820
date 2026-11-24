// Command bptool is the inspection front-end for the disk B+ tree.
//
// Subcommands:
//
//	bptool demo [-n keys] [-page bytes] [-path file]
//	    Build a tree from fixed-seed watch-part codes, run a mixed workload,
//	    print per-level page/fanout stats, per-page internals, split events
//	    and the before/after compression size comparison.
//
//	bptool stats -path file
//	    Open an existing page file and print aggregate statistics.
//
//	bptool dump  -path file [-page id]
//	    Inspect one page (or every page) showing the common prefix, the
//	    compressed on-page records and reconstructed full keys.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"

	"bptree/bptree"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "demo":
		runDemo(os.Args[2:])
	case "stats":
		runStats(os.Args[2:])
	case "dump":
		runDump(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `bptool — disk B+ tree inspector

usage:
  bptool demo  [-n N] [-page BYTES] [-path FILE] [--disk]
  bptool stats -path FILE
  bptool dump  -path FILE [-page ID]
`)
}

func runDemo(args []string) {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	n := fs.Int("n", 200_000, "number of distinct part codes")
	pageSize := fs.Int("page", bptree.DefaultPageSize, "page size in bytes")
	path := fs.String("path", "", "optional page file; defaults to an in-memory pager")
	useDisk := fs.Bool("disk", false, "force a disk-backed pager (temp file when -path empty)")
	mutations := fs.Int("mutations", 40_000, "mixed delete/reinsert operations after the bulk load")
	_ = fs.Parse(args)

	var pg bptree.Pager
	if *useDisk || *path != "" {
		file := *path
		if file == "" {
			file = "bptree-demo.db"
		}
		fp, err := bptree.NewFilePager(file, *pageSize, 256)
		if err != nil {
			fatal(err)
		}
		pg = fp
	} else {
		pg = bptree.NewMemoryPager(*pageSize)
	}
	tree := bptree.Open(pg)

	rng := rand.New(rand.NewSource(20261124))
	keys := make([][]byte, *n)
	for i := range keys {
		keys[i] = bptree.GenPartKey(rng, i)
	}
	fmt.Printf("building index: %d part codes, page size %d bytes\n", *n, *pageSize)
	order := rng.Perm(*n)
	for i, idx := range order {
		if err := tree.Put(keys[idx], []byte(randValue(rng))); err != nil {
			fatal(err)
		}
		if (*n >= 50_000) && (i+1)%(*n/5) == 0 {
			fmt.Printf("  inserted %d/%d\n", i+1, *n)
		}
	}
	// Mixed workload to create splits, deletes and compaction.
	for i := 0; i < *mutations; i++ {
		k := keys[rng.Intn(*n)]
		if i%3 == 0 {
			_ = tree.Delete(k)
		} else {
			_ = tree.Put(k, []byte(randValue(rng)))
		}
	}
	if err := tree.CheckInvariants(); err != nil {
		fatal(err)
	}
	printStats(tree)
	printSplitSample(tree)
	printPageSample(tree)
	if cl, ok := pg.(*bptree.FilePager); ok {
		if err := tree.Close(); err != nil {
			fatal(err)
		}
		fmt.Printf("\npersisted to %s\n", cl.Name())
	} else {
		_ = tree.Close()
	}
}

func runStats(args []string) {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	path := fs.String("path", "", "page file")
	_ = fs.Parse(args)
	if *path == "" {
		fatal(fmt.Errorf("stats requires -path"))
	}
	pg, err := bptree.NewFilePager(*path, bptree.DefaultPageSize, 256)
	if err != nil {
		fatal(err)
	}
	tree := bptree.Open(pg)
	printStats(tree)
	_ = tree.Close()
}

func runDump(args []string) {
	fs := flag.NewFlagSet("dump", flag.ExitOnError)
	path := fs.String("path", "", "page file")
	pageID := fs.Int("page", -1, "page id; -1 dumps the whole tree breadth-first")
	_ = fs.Parse(args)
	if *path == "" {
		fatal(fmt.Errorf("dump requires -path"))
	}
	pg, err := bptree.NewFilePager(*path, bptree.DefaultPageSize, 256)
	if err != nil {
		fatal(err)
	}
	tree := bptree.Open(pg)
	if *pageID < 0 {
		for _, pv := range tree.InspectTree() {
			printPage(pv)
		}
	} else {
		// Resolve the leaf-relative level via a full-tree view so a single
		// interior page dump shows the same level numbering as a full dump.
		levelByID := map[int]int{}
		for _, pv := range tree.InspectTree() {
			levelByID[pv.ID] = pv.Level
		}
		pv := tree.InspectPage(*pageID)
		if lvl, ok := levelByID[pv.ID]; ok {
			pv.Level = lvl
		}
		printPage(pv)
	}
	_ = tree.Close()
}

func printStats(tree *bptree.Tree) {
	st := tree.Stats()
	fmt.Println("\n=== 索引总览 / index overview ===")
	fmt.Printf("tree height (levels) : %d\n", st.Height)
	fmt.Printf("leaf pages           : %d\n", st.LeafPages)
	fmt.Printf("interior pages       : %d\n", st.InteriorPages)
	fmt.Printf("overflow pages       : %d\n", st.OverflowPages)
	fmt.Printf("free pages           : %d\n", st.FreePages)
	fmt.Println("\nlevel  pages  minFan  avgFan  maxFan  minCP  avgCP  maxCP")
	for _, lv := range st.Levels {
		fmt.Printf("%5d  %5d  %6d  %6.1f  %6d  %5d  %5.1f  %5d\n",
			lv.Level, lv.Pages, lv.MinFanout, lv.AvgFanout, lv.MaxFanout,
			lv.MinPrefix, lv.AvgPrefix, lv.MaxPrefix)
	}
	fmt.Println("\n=== 压缩前后 / compression ===")
	fmt.Printf("logical key+value bytes : %d\n", st.LogicalBytes)
	fmt.Printf("on-disk bytes used      : %d\n", st.OnDiskBytes)
	fmt.Printf("on-disk / logical ratio : %.3f (lower = better)\n", st.CompressionRate)
	fmt.Printf("average leaf fill       : %.1f%%\n", st.AvgLeafFill*100)
}

func printSplitSample(tree *bptree.Tree) {
	evs := tree.Splits()
	fmt.Println("\n=== 分裂分隔键样本 / split separator sample ===")
	limit := 8
	if len(evs) < limit {
		limit = len(evs)
	}
	fmt.Println("level  chosen  window       entries  sepLen  separator")
	for i := 0; i < limit; i++ {
		ev := evs[len(evs)-1-i]
		fmt.Printf("%5d  %6d  [%3d,%3d]  %7d  %6d  %q\n",
			ev.Level, ev.ChosenAt, ev.WindowLow, ev.WindowHigh,
			ev.EntriesOnPage, ev.SeparatorLen, truncate(ev.Separator, 40))
	}
	fmt.Printf("(%d split events retained)\n", len(evs))
}

func printPageSample(tree *bptree.Tree) {
	views := tree.InspectTree()
	fmt.Println("\n=== 页面内部样本 / page internals sample ===")
	shown := 0
	for _, pv := range views {
		if shown >= 3 {
			break
		}
		printPage(pv)
		shown++
	}
}

func printPage(pv bptree.PageView) {
	fmt.Printf("\n--- page %d (%s, level %d) ---\n", pv.ID, pv.Type, pv.Level)
	fmt.Printf("records        : %d\n", pv.NKeys)
	fmt.Printf("common prefix  : %q (len %d)\n", truncate(pv.CommonPrefix, 48), len(pv.CommonPrefix))
	if pv.Type == "leaf" {
		fmt.Printf("siblings       : prev=%d next=%d\n", pv.PrevSibling, pv.NextSibling)
	} else {
		fmt.Printf("first child    : %d\n", pv.FirstChild)
	}
	fmt.Printf("bytes used/free: %d / %d\n", pv.BytesUsed, pv.FreeSpace)
	if pv.Type == "leaf" {
		fmt.Printf("compaction      : every mutation re-encodes the page, so dead records are reclaimed immediately; fragmentation=%d\n", pv.Fragmentation)
	}
	for i, rec := range pv.StoredRecords {
		if i >= 6 {
			fmt.Printf("  ... (%d more records)\n", len(pv.StoredRecords)-i)
			break
		}
		fmt.Printf("  [%d] %s\n", i, rec)
	}
	for i, s := range pv.Separators {
		if i >= 6 {
			break
		}
		fmt.Printf("  separator %q shortest-now=%v\n", truncate(s.Separator, 40), s.Shortest)
	}
}

func truncate(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return append(append([]byte(nil), b[:n]...), '.', '.', '.')
}

func randValue(rng *rand.Rand) string {
	b := make([]byte, 8+rng.Intn(24))
	for i := range b {
		b[i] = byte('A' + rng.Intn(26))
	}
	return string(b)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "bptool:", err)
	os.Exit(1)
}
