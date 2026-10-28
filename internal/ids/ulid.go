// Package ids generates ULIDs: 48-bit millisecond timestamp plus 80 random bits, Crockford base32.
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"
)

const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var (
	mu       sync.Mutex
	lastMS   uint64
	lastRand [10]byte
)

// New returns a ULID that sorts after any ULID previously returned by this process.
func New() string {
	mu.Lock()
	defer mu.Unlock()
	ms := uint64(time.Now().UnixMilli())
	if ms <= lastMS {
		ms = lastMS
		increment(&lastRand)
	} else {
		lastMS = ms
		_, _ = rand.Read(lastRand[:])
	}
	var b [16]byte
	binary.BigEndian.PutUint64(b[0:8], ms<<16)
	copy(b[6:], lastRand[:])
	return encode(b)
}

func increment(r *[10]byte) {
	for i := len(r) - 1; i >= 0; i-- {
		r[i]++
		if r[i] != 0 {
			return
		}
	}
}

func encode(b [16]byte) string {
	out := make([]byte, 26)
	hi := binary.BigEndian.Uint64(b[0:8])
	lo := binary.BigEndian.Uint64(b[8:16])
	for i := 25; i >= 0; i-- {
		out[i] = alphabet[lo&31]
		lo = lo>>5 | (hi&31)<<59
		hi >>= 5
	}
	return string(out)
}
