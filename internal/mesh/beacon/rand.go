package beacon

import "crypto/rand"

// randRead is a seam so tests can make beacon nonces deterministic.
var randRead = rand.Read
