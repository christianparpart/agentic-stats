package seal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"hash"
)

// hmacNew is a small indirection so the MAC construction is stated once.
func hmacNew(key []byte) hash.Hash { return hmac.New(sha256.New, key) }

// putUint64 writes a big-endian length prefix.
func putUint64(b []byte, v uint64) { binary.BigEndian.PutUint64(b, v) }
