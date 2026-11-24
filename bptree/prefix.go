package bptree

import "bytes"

// shortestSeparator returns the shortest byte string s such that
//
//	left < s && s <= right   (with left < right given).
//
// Let p = LCP(left, right). A separator of length p cannot exist: a length-p
// string is either <= the shared prefix (hence <= left) or above it (hence
// above right, which starts with that prefix). So the minimum length is p+1.
//
// Among length p+1 candidates right[:p]+x with x <= right[p], pick the
// smallest x that still beats left:
//
//   - left is a strict prefix of right (p == len(left)): end-of-key sorts
//     below every byte, so the smallest possible x is 0, provided 0 is also
//     <= right[p]. Example: ("ab","abcd") -> "ab\x00". For readability the
//     caller-facing tests use printable data; the tree stores the raw bytes.
//   - otherwise the keys differ at p: the smallest beating byte is
//     left[p]+1; if that stays <= right[p] use it ("az","ba" -> "b"),
//     else the only choice is right[:p+1] ("abc","abd" -> "abd").
func shortestSeparator(left, right []byte) []byte {
	p := commonPrefixLen(left, right)
	s := make([]byte, p+1)
	copy(s, right[:p])
	switch {
	case p == len(left):
		// Any byte x works as long as x <= right[p]; 0 is the smallest.
		if 0 <= right[p] { // always true for a byte
			s[p] = 0
		}
		// Edge case: right[p] would have to be < 0 to reject, impossible.
		// But a separator ending in 0x00 can equal a real key boundary; the
		// strict inequality left < s still holds because left is shorter.
	default:
		if left[p]+1 <= right[p] {
			s[p] = left[p] + 1
		} else {
			s[p] = right[p]
		}
	}
	return s
}

// compareBytes compares a and b lexicographically, returning -1/0/1. The
// comparison stops at the first differing byte, so long keys that share a
// large prefix only pay for the differing tail (requirement 7). This is a
// named wrapper around the standard library implementation so the intent
// stays explicit at every call site.
func compareBytes(a, b []byte) int {
	// bytes.Compare loops only until the first differing byte, so long
	// look-alike keys only pay for the bytes after their shared prefix.
	return bytes.Compare(a, b)
}

// compareKeySuffix compares (cp+suffix) against key without materializing
// the concatenation. It first walks the common page prefix, then continues
// at the first byte that could differ — again no work is spent on the
// shared prefix.
func compareKeySuffix(cp, suffix, key []byte) int {
	if len(key) < len(cp) {
		if c := bytes.Compare(key, cp[:len(key)]); c != 0 {
			return c
		}
		// key is a strict prefix of cp, so key < cp+suffix.
		return -1
	}
	if c := bytes.Compare(key[:len(cp)], cp); c != 0 {
		return c
	}
	return bytes.Compare(key[len(cp):], suffix)
}

// prefixSuccessor returns the smallest key strictly greater than every key
// having `prefix` as prefix. It returns nil when no such finite key exists
// (prefix is all 0xFF bytes), which scans interpret as "no upper bound".
func prefixSuccessor(prefix []byte) []byte {
	i := len(prefix) - 1
	for i >= 0 && prefix[i] == 0xFF {
		i--
	}
	if i < 0 {
		return nil
	}
	out := make([]byte, i+1)
	copy(out, prefix[:i+1])
	out[i]++
	return out
}
