package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wallet-ledger/internal/ledger"
)

const testToken = "unit-test-token"

// fakeLedger records whether storage was touched.
type fakeLedger struct {
	calls   int
	balance int64
	err     error
}

func (f *fakeLedger) Apply(_ context.Context, _ ledger.Op, _ string, amount int64, _ string) (int64, error) {
	f.calls++
	return f.balance + amount, f.err
}
func (f *fakeLedger) Balance(context.Context, string) (int64, error) {
	f.calls++
	return f.balance, f.err
}
func (f *fakeLedger) Ping(context.Context) error { f.calls++; return f.err }

func newTestServer(f *fakeLedger) *Server {
	return New(f, testToken, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func do(s *Server, method, path, body, auth string) (*httptest.ResponseRecorder, map[string]any) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec, m
}

func TestAuthRejectedBeforeStorage(t *testing.T) {
	cases := []string{
		"",
		"Bearer",
		"Bearer ",
		"Basic " + testToken,
		"Bearer wrong-token",
		"Bearer " + testToken + "x",
		"Bearer " + testToken[:len(testToken)-1],
		"Token " + testToken,
	}
	paths := []struct{ method, path, body string }{
		{"POST", "/credit", `{"user_id":"u1","amount":1}`},
		{"POST", "/debit", `{"user_id":"u1","amount":1}`},
		{"GET", "/balance/u1", ""},
		{"GET", "/health", ""},
		{"GET", "/nope", ""},
		{"POST", "/credit", `not json`},
	}
	for _, auth := range cases {
		for _, p := range paths {
			f := &fakeLedger{}
			rec, m := do(newTestServer(f), p.method, p.path, p.body, auth)
			if rec.Code != 401 || m["error"] != "unauthorized" {
				t.Errorf("auth=%q %s %s: got %d %v", auth, p.method, p.path, rec.Code, m)
			}
			if f.calls != 0 {
				t.Errorf("auth=%q %s %s: storage was called before auth", auth, p.method, p.path)
			}
		}
	}
}

func TestAuthAccepted(t *testing.T) {
	for _, auth := range []string{"Bearer " + testToken, "bearer " + testToken, "BEARER " + testToken} {
		rec, _ := do(newTestServer(&fakeLedger{balance: 5}), "GET", "/balance/u1", "", auth)
		if rec.Code != 200 {
			t.Errorf("auth=%q: got %d", auth, rec.Code)
		}
	}
}

func TestValidation(t *testing.T) {
	bad := []string{
		``,
		`{`,
		`null`,
		`[]`,
		`"x"`,
		`{"user_id":"u1","amount":1}{}`,
		`{"amount":1}`,
		`{"user_id":"u1"}`,
		`{"user_id":"","amount":1}`,
		`{"user_id":null,"amount":1}`,
		`{"user_id":123,"amount":1}`,
		`{"user_id":"has space","amount":1}`,
		`{"user_id":"dot.not.allowed","amount":1}`,
		`{"user_id":"ünicode","amount":1}`,
		`{"user_id":"` + strings.Repeat("a", 65) + `","amount":1}`,
		`{"user_id":"u1","amount":0}`,
		`{"user_id":"u1","amount":-1}`,
		`{"user_id":"u1","amount":1.5}`,
		`{"user_id":"u1","amount":100.0}`,
		`{"user_id":"u1","amount":1e3}`,
		`{"user_id":"u1","amount":"100"}`,
		`{"user_id":"u1","amount":null}`,
		`{"user_id":"u1","amount":true}`,
		`{"user_id":"u1","amount":1000000001}`,
		`{"user_id":"u1","amount":99999999999999999999}`,
	}
	for _, body := range bad {
		for _, path := range []string{"/credit", "/debit"} {
			f := &fakeLedger{}
			rec, m := do(newTestServer(f), "POST", path, body, "Bearer "+testToken)
			if rec.Code != 400 || m["error"] != "invalid_request" {
				t.Errorf("%s %q: got %d %v", path, body, rec.Code, m)
			}
			if f.calls != 0 {
				t.Errorf("%s %q: storage called for invalid input", path, body)
			}
		}
	}

	good := []string{
		`{"user_id":"u1","amount":1}`,
		`{"user_id":"u1","amount":1000000000}`,
		`{"user_id":"A-z_0-9","amount":5, "unknown": {"x": [1,2]}}`,
		`{"user_id":"` + strings.Repeat("a", 64) + `","amount":1}`,
		"  {\"amount\": 7 , \"user_id\": \"u1\"}\n",
	}
	for _, body := range good {
		rec, m := do(newTestServer(&fakeLedger{}), "POST", "/credit", body, "Bearer "+testToken)
		if rec.Code != 200 {
			t.Errorf("%q: got %d %v", body, rec.Code, m)
		}
	}
}

func TestBalancePathValidation(t *testing.T) {
	for _, path := range []string{"/balance/", "/balance/a%20b", "/balance/a/b", "/balance/" + strings.Repeat("x", 65)} {
		rec, m := do(newTestServer(&fakeLedger{}), "GET", path, "", "Bearer "+testToken)
		if rec.Code != 400 || m["error"] != "invalid_request" {
			t.Errorf("%s: got %d %v", path, rec.Code, m)
		}
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{ledger.ErrUserNotFound, 404, "user_not_found"},
		{ledger.ErrInsufficientFunds, 409, "insufficient_funds"},
		{ledger.ErrIdempotencyKeyReused, 422, "idempotency_key_reused"},
	}
	for _, c := range cases {
		rec, m := do(newTestServer(&fakeLedger{err: c.err}), "POST", "/debit", `{"user_id":"u1","amount":1}`, "Bearer "+testToken)
		if rec.Code != c.status || m["error"] != c.code {
			t.Errorf("%v: got %d %v", c.err, rec.Code, m)
		}
		if _, ok := m["message"].(string); !ok {
			t.Errorf("%v: missing message", c.err)
		}
	}
}

func TestSuccessShape(t *testing.T) {
	rec, m := do(newTestServer(&fakeLedger{balance: 15000}), "POST", "/credit", `{"user_id":"u123","amount":10000}`, "Bearer "+testToken)
	if rec.Code != 200 || m["user_id"] != "u123" || m["balance"] != float64(25000) {
		t.Fatalf("got %d %v", rec.Code, m)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type %q", ct)
	}
}

func TestMethodAndRouting(t *testing.T) {
	s := newTestServer(&fakeLedger{})
	if rec, _ := do(s, "GET", "/credit", "", "Bearer "+testToken); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /credit: %d", rec.Code)
	}
	if rec, _ := do(s, "POST", "/balance/u1", "", "Bearer "+testToken); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /balance: %d", rec.Code)
	}
	if rec, m := do(s, "GET", "/unknown", "", "Bearer "+testToken); rec.Code != 404 || m["error"] != "not_found" {
		t.Errorf("unknown path: %d %v", rec.Code, m)
	}
}

func TestIdempotencyKeyValidation(t *testing.T) {
	s := newTestServer(&fakeLedger{})
	req := httptest.NewRequest("POST", "/credit", strings.NewReader(`{"user_id":"u1","amount":1}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Idempotency-Key", strings.Repeat("k", 256))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("expected 400 for oversized key, got %d", rec.Code)
	}
}
