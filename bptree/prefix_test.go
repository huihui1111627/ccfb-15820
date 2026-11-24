package bptree

import (
	"bytes"
	"testing"
)

func TestShortestSeparator(t *testing.T) {
	cases := []struct{ left, right, want string }{
		{"abc", "abd", "abd"},    // adjacent differing bytes: no intermediate byte
		{"az", "ba", "b"},        // 'z'+1='a'+255+1 wraps to 'b' effectively: a+1
		{"ab", "abcd", "ab\x00"}, // left is prefix: smallest beating byte is 0x00
		{"x\xffz", "y00", "y"},   // left[p]=0xFF: left[p]+1 overflows, fall back to right[:p+1]
		{"", "a", "\x00"},        // empty left: 0x00 separates
		{"AAAABRAND20240101SN000001", "AAAABRAND20240101SN000002", "AAAABRAND20240101SN000002"},
		{"ROLEX_SUB_2024_A", "ROLEX_SUB_2024_B", "ROLEX_SUB_2024_B"},
	}
	for _, c := range cases {
		got := shortestSeparator([]byte(c.left), []byte(c.right))
		if string(got) != c.want {
			t.Fatalf("shortestSeparator(%q,%q)=%q want %q", c.left, c.right, got, c.want)
		}
		if bytes.Compare([]byte(c.left), got) >= 0 {
			t.Fatalf("separator %q not > left %q", got, c.left)
		}
		if bytes.Compare(got, []byte(c.right)) > 0 {
			t.Fatalf("separator %q not <= right %q", got, c.right)
		}
		// Minimality: dropping the last byte must fail to separate.
		shorter := got[:len(got)-1]
		if bytes.Compare([]byte(c.left), shorter) < 0 && bytes.Compare(shorter, []byte(c.right)) <= 0 {
			t.Fatalf("separator %q not minimal; %q also separates", got, shorter)
		}
	}
}

func TestPrefixSuccessor(t *testing.T) {
	if got := prefixSuccessor([]byte("abc")); string(got) != "abd" {
		t.Fatalf("got %q", got)
	}
	if got := prefixSuccessor([]byte("a\xff")); string(got) != "b" {
		t.Fatalf("got %q", got)
	}
	if got := prefixSuccessor([]byte{0xFF, 0xFF}); got != nil {
		t.Fatalf("all-FF prefix must return nil, got %q", got)
	}
}

func TestCommonPrefix(t *testing.T) {
	if commonPrefixLen([]byte("abcde"), []byte("abcf")) != 3 {
		t.Fatal("lcp wrong")
	}
	if commonPrefixAll([][]byte{[]byte("abc"), []byte("abd")}) != 2 {
		t.Fatal("lcp all wrong")
	}
	if commonPrefixAll(nil) != 0 {
		t.Fatal("empty lcp must be 0")
	}
}
