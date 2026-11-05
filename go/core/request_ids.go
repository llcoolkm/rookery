package core

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// NewRequestID generates an RFC 9562 UUIDv7 with cryptographic random bits.
// The injected timestamp permits deterministic contract checks; production uses
// the current clock. UUIDv7 order within one millisecond is not guaranteed.
func NewRequestID(now time.Time) (string, error) {
	ms := now.UnixMilli()
	if ms < 0 || ms >= 1<<48 {
		return "", fmt.Errorf("request timestamp outside UUIDv7 range")
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// RequestCreated extracts UUIDv7 time; legacy canonical UUIDs return nil.
// Acceptance windows and legacy migration deadlines are hub policy (phase 2).
func RequestCreated(id string) (*time.Time, error) {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return nil, fmt.Errorf("ID must be a canonical UUID")
	}
	raw := id[:8] + id[9:13] + id[14:18] + id[19:23] + id[24:]
	b, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("ID must be a UUID")
	}
	if fmt.Sprintf("%x", b) != raw {
		return nil, fmt.Errorf("ID must be a canonical UUID")
	}
	if b[6]>>4 != 7 || b[8]>>6 != 2 {
		return nil, nil
	}
	var ms int64
	for _, v := range b[:6] {
		ms = ms<<8 | int64(v)
	}
	t := time.UnixMilli(ms)
	return &t, nil
}
