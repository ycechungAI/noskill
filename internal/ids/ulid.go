// Package ids generates ULIDs: sortable, offline IDs for runs, approvals, jobs and memory items
// (DATA_MODEL.md "Rules").
package ids

import (
	"crypto/rand"
	"sync"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var (
	mu       sync.Mutex
	lastMs   uint64
	lastRand [10]byte
)

// New returns a 26-character ULID. IDs made in the same millisecond stay in order: the random
// part is incremented instead of redrawn.
func New() string { return at(time.Now()) }

func at(t time.Time) string {
	ms := uint64(t.UnixMilli())
	mu.Lock()
	if ms == lastMs {
		for i := len(lastRand) - 1; i >= 0; i-- {
			lastRand[i]++
			if lastRand[i] != 0 {
				break
			}
		}
	} else {
		lastMs = ms
		if _, err := rand.Read(lastRand[:]); err != nil {
			panic("ids: no randomness: " + err.Error())
		}
	}
	var b [16]byte
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (40 - 8*i))
	}
	copy(b[6:], lastRand[:])
	mu.Unlock()
	return encode(b)
}

// encode writes 128 bits as 26 Crockford base32 characters (the first carries 3 bits).
func encode(b [16]byte) string {
	var out [26]byte
	// Treat b as a big-endian 128-bit number and take 5 bits at a time from the low end.
	hi := uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
	lo := uint64(b[8])<<56 | uint64(b[9])<<48 | uint64(b[10])<<40 | uint64(b[11])<<32 |
		uint64(b[12])<<24 | uint64(b[13])<<16 | uint64(b[14])<<8 | uint64(b[15])
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}
