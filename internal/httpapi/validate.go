package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
)

const (
	MaxAmount        = 1_000_000_000
	maxUserIDLen     = 64
	maxIdemKeyLen    = 255
	maxBodyBytes     = 64 << 10
	msgAmountInvalid = "amount must be a JSON integer between 1 and 1000000000"
	msgUserInvalid   = "user_id must be a string of 1-64 characters from A-Z a-z 0-9 _ -"
)

type opRequest struct {
	UserID string
	Amount int64
}

// validUserID: 1..64 bytes of [A-Za-z0-9_-]. Checked byte-wise, so any
// multi-byte UTF-8 character is rejected too.
func validUserID(s string) bool {
	if len(s) < 1 || len(s) > maxUserIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// validIdempotencyKey: 1..255 visible ASCII characters.
func validIdempotencyKey(s string) bool {
	if len(s) < 1 || len(s) > maxIdemKeyLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// parseOpRequest validates a credit/debit body. It returns a non-empty
// message when the body breaks a rule.
//
// The body is decoded into raw fields first so the exact JSON token of
// "amount" can be inspected: 100, and only 100, is accepted, while 100.0,
// 1e2, "100", true and null are all rejected. Unknown fields are ignored.
func parseOpRequest(body []byte) (opRequest, string) {
	var fields map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&fields); err != nil {
		return opRequest{}, "request body must be a valid JSON object"
	}
	if fields == nil { // literal null
		return opRequest{}, "request body must be a valid JSON object"
	}
	if _, err := dec.Token(); err != io.EOF {
		return opRequest{}, "request body must contain a single JSON object"
	}

	rawUser, ok := fields["user_id"]
	if !ok {
		return opRequest{}, "user_id is required"
	}
	var userID string
	if len(rawUser) == 0 || rawUser[0] != '"' || json.Unmarshal(rawUser, &userID) != nil || !validUserID(userID) {
		return opRequest{}, msgUserInvalid
	}

	rawAmount, ok := fields["amount"]
	if !ok {
		return opRequest{}, "amount is required"
	}
	amount, ok := parseAmount(rawAmount)
	if !ok {
		return opRequest{}, msgAmountInvalid
	}
	return opRequest{UserID: userID, Amount: amount}, ""
}

// parseAmount accepts only a plain positive JSON integer token in range.
func parseAmount(raw json.RawMessage) (int64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > 10 { // 1000000000 has 10 digits
		return 0, false
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, false // '-', '.', 'e', quotes, letters...
		}
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || n < 1 || n > MaxAmount {
		return 0, false
	}
	return n, true
}
