package service

import (
	"crypto/rand"
	"encoding/binary"
)

// randUint16 returns a cryptographically random 16-bit value, used to build
// collision-free download ids.
func randUint16() uint16 {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	return binary.BigEndian.Uint16(b[:])
}
