package ids

import (
	"sort"
	"testing"
	"time"
)

func TestOrderedWithinMillisecond(t *testing.T) {
	now := time.UnixMilli(1_760_000_000_000)
	var got []string
	for i := 0; i < 1000; i++ {
		got = append(got, at(now))
	}
	if !sort.StringsAreSorted(got) {
		t.Fatal("ULIDs from the same millisecond are not sorted")
	}
	if len(got[0]) != 26 {
		t.Fatalf("len = %d, want 26", len(got[0]))
	}
}

func TestOrderedAcrossTime(t *testing.T) {
	a := at(time.UnixMilli(1_000))
	b := at(time.UnixMilli(2_000))
	if a >= b {
		t.Fatalf("%s should sort before %s", a, b)
	}
}

func TestTimestampPrefix(t *testing.T) {
	// 0 ms → the first 10 characters (48 bits of time) are all zero.
	if got := at(time.UnixMilli(0))[:10]; got != "0000000000" {
		t.Fatalf("prefix = %s", got)
	}
}
