package proxy

import (
	"crypto/rand"
	"encoding/hex"
)

// newID returns a short random id for request correlation.
func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// recID returns a log-record id.
func recID() string {
	return "r_" + newID()
}