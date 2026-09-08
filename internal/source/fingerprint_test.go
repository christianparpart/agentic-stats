package source

import "hash/fnv"

// fingerprintOf mirrors FileStream.fingerprint for test expectations.
func fingerprintOf(content string) uint64 {
	b := []byte(content)
	if len(b) > fingerprintSize {
		b = b[:fingerprintSize]
	}
	if len(b) == 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write(b)
	return h.Sum64()
}
