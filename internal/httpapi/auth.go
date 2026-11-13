package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"strings"
)

// authenticator checks "Authorization: Bearer <token>".
//
// Both sides are hashed to fixed-length digests before the constant-time
// compare, so neither the content nor the length of the expected token leaks
// through timing. Only the digest is kept in memory after start-up.
type authenticator struct {
	want [sha256.Size]byte
}

func newAuthenticator(token string) authenticator {
	return authenticator{want: sha256.Sum256([]byte(token))}
}

func (a authenticator) allowed(header string) bool {
	const scheme = "Bearer "
	// The auth scheme is case-insensitive (RFC 9110 §11.1); the token is not.
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return false
	}
	got := sha256.Sum256([]byte(header[len(scheme):]))
	return subtle.ConstantTimeCompare(got[:], a.want[:]) == 1
}
