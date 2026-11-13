// Package e2e runs black-box tests against a running API (and, when
// DATABASE_URL is set, checks the database directly).
//
//	docker compose --profile tools run --rm tests
//	# or from the host:
//	BASE_URL=http://localhost:8080 DATABASE_URL=postgres://ledger:ledger@localhost:5432/ledger go test -v ./e2e
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	baseURL = envOr("BASE_URL", "http://localhost:8080")
	token   = envOr("API_TOKEN", "recruitment-test-token")
	client  = &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        2000,
			MaxIdleConnsPerHost: 2000,
			IdleConnTimeout:     30 * time.Second,
		},
	}
	runID   = fmt.Sprintf("%x", time.Now().UnixNano()&0xffffffffff)
	userSeq atomic.Int64
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// newUser returns a user id unique to this test run.
func newUser(prefix string) string {
	return fmt.Sprintf("t_%s_%s_%d", prefix, runID, userSeq.Add(1))
}

type result struct {
	Status  int    `json:"-"`
	UserID  string `json:"user_id"`
	Balance int64  `json:"balance"`
	Error   string `json:"error"`
	Message string `json:"message"`
	Raw     string `json:"-"`
}

func request(method, path, body string, headers map[string]string) (result, error) {
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, baseURL+path, rdr)
	if err != nil {
		return result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return result{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return result{}, err
	}
	r := result{Status: resp.StatusCode, Raw: string(raw)}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("status %d, body is not JSON: %q", resp.StatusCode, raw)
	}
	return r, nil
}

func opBody(user string, amount int64) string {
	return fmt.Sprintf(`{"user_id":%q,"amount":%d}`, user, amount)
}

func credit(user string, amount int64) (result, error) {
	return request("POST", "/credit", opBody(user, amount), nil)
}
func debit(user string, amount int64) (result, error) {
	return request("POST", "/debit", opBody(user, amount), nil)
}
func balance(user string) (result, error) {
	return request("GET", "/balance/"+user, "", nil)
}

func must(t testing.TB, r result, err error) result {
	t.Helper()
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return r
}

func expect(t testing.TB, r result, err error, status int, code string) result {
	t.Helper()
	r = must(t, r, err)
	if r.Status != status || (code != "" && r.Error != code) {
		t.Fatalf("expected %d %s, got %d %s", status, code, r.Status, r.Raw)
	}
	if status >= 400 && r.Message == "" {
		t.Fatalf("error response without message: %s", r.Raw)
	}
	return r
}

func expectBalance(t testing.TB, user string, want int64) {
	t.Helper()
	r, err := balance(user)
	r = expect(t, r, err, 200, "")
	if r.Balance != want || r.UserID != user {
		t.Fatalf("balance of %s: want %d, got %s", user, want, r.Raw)
	}
}

// db returns a pool or skips the test when DATABASE_URL is not set.
func db(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func percentile(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(float64(len(s))*p+0.999999) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}

func randAmount(max int64) int64 { return 1 + rand.Int64N(max) }
