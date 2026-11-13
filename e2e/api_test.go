package e2e

import (
	"strings"
	"testing"
)

func TestAuthOnEveryEndpoint(t *testing.T) {
	user := newUser("auth")
	endpoints := []struct{ method, path, body string }{
		{"POST", "/credit", opBody(user, 100)},
		{"POST", "/debit", opBody(user, 100)},
		{"GET", "/balance/" + user, ""},
		{"GET", "/health", ""},
		{"GET", "/does-not-exist", ""},
		{"POST", "/credit", "{not json"},
	}
	headers := map[string]string{
		"missing":      "",
		"wrong scheme": "Basic " + token,
		"wrong token":  "Bearer " + token + "-nope",
		"empty token":  "Bearer ",
		"no space":     "Bearer" + token,
		"token only":   token,
	}
	for name, h := range headers {
		for _, e := range endpoints {
			r, err := request(e.method, e.path, e.body, map[string]string{"Authorization": h})
			expect(t, r, err, 401, "unauthorized")
			if strings.Contains(r.Raw, token) {
				t.Fatalf("%s: response leaks the token", name)
			}
		}
	}
	// Nothing may have been written by the rejected credit.
	r, err := balance(user)
	expect(t, r, err, 404, "user_not_found")
}

func TestHealth(t *testing.T) {
	r, err := request("GET", "/health", "", nil)
	must(t, r, err)
	if r.Status != 200 || !strings.Contains(r.Raw, `"ok"`) {
		t.Fatalf("health: %d %s", r.Status, r.Raw)
	}
}

func TestInputValidation(t *testing.T) {
	user := newUser("val")
	bad := []string{
		``,
		`{`,
		`null`,
		`[]`,
		`"just a string"`,
		`{"amount":100}`,
		`{"user_id":"` + user + `"}`,
		`{"user_id":"","amount":100}`,
		`{"user_id":"` + strings.Repeat("x", 65) + `","amount":100}`,
		`{"user_id":"bad id","amount":100}`,
		`{"user_id":"bad/id","amount":100}`,
		`{"user_id":"bäd","amount":100}`,
		`{"user_id":42,"amount":100}`,
		`{"user_id":"` + user + `","amount":0}`,
		`{"user_id":"` + user + `","amount":-5}`,
		`{"user_id":"` + user + `","amount":10.5}`,
		`{"user_id":"` + user + `","amount":100.0}`,
		`{"user_id":"` + user + `","amount":1e2}`,
		`{"user_id":"` + user + `","amount":"100"}`,
		`{"user_id":"` + user + `","amount":null}`,
		`{"user_id":"` + user + `","amount":1000000001}`,
		`{"user_id":"` + user + `","amount":18446744073709551616}`,
	}
	for _, path := range []string{"/credit", "/debit"} {
		for _, body := range bad {
			r, err := request("POST", path, body, nil)
			must(t, r, err)
			if r.Status != 400 || r.Error != "invalid_request" {
				t.Errorf("%s %s: expected 400 invalid_request, got %d %s", path, body, r.Status, r.Raw)
			}
		}
	}
	for _, path := range []string{"/balance/bad%20id", "/balance/" + strings.Repeat("x", 65), "/balance/"} {
		r, err := request("GET", path, "", nil)
		expect(t, r, err, 400, "invalid_request")
	}
	// None of the above created the user.
	r, err := balance(user)
	expect(t, r, err, 404, "user_not_found")

	// Boundaries and unknown fields are accepted.
	r, err = request("POST", "/credit", `{"user_id":"`+user+`","amount":1000000000,"note":"ignored","x":{"y":[1]}}`, nil)
	if r = expect(t, r, err, 200, ""); r.Balance != 1_000_000_000 {
		t.Fatalf("got %s", r.Raw)
	}
	r, err = credit(user, 1)
	if r = expect(t, r, err, 200, ""); r.Balance != 1_000_000_001 {
		t.Fatalf("got %s", r.Raw)
	}
	long := strings.Repeat("Az09_-", 10) + "abcd" // 64 chars
	r, err = credit(long, 5)
	expect(t, r, err, 200, "")
}

func TestContract(t *testing.T) {
	u := newUser("flow")

	r, err := balance(u)
	expect(t, r, err, 404, "user_not_found")
	r, err = debit(u, 1)
	expect(t, r, err, 404, "user_not_found")

	r, err = credit(u, 15000)
	if r = expect(t, r, err, 200, ""); r.Balance != 15000 || r.UserID != u {
		t.Fatalf("first credit: %s", r.Raw)
	}
	r, err = credit(u, 10000)
	if r = expect(t, r, err, 200, ""); r.Balance != 25000 {
		t.Fatalf("second credit: %s", r.Raw)
	}
	r, err = debit(u, 4000)
	if r = expect(t, r, err, 200, ""); r.Balance != 21000 {
		t.Fatalf("debit: %s", r.Raw)
	}
	expectBalance(t, u, 21000)

	r, err = debit(u, 21001)
	expect(t, r, err, 409, "insufficient_funds")
	expectBalance(t, u, 21000)

	r, err = debit(u, 21000) // exactly to zero is allowed
	if r = expect(t, r, err, 200, ""); r.Balance != 0 {
		t.Fatalf("debit to zero: %s", r.Raw)
	}
	r, err = debit(u, 1)
	expect(t, r, err, 409, "insufficient_funds")
	expectBalance(t, u, 0)

	// Method checks.
	r, err = request("GET", "/credit", "", nil)
	must(t, r, err)
	if r.Status != 405 {
		t.Fatalf("GET /credit: %d", r.Status)
	}
}

func TestIdempotencyKey(t *testing.T) {
	u := newUser("idem")
	key := map[string]string{"Idempotency-Key": "k-" + u}

	first, err := request("POST", "/credit", opBody(u, 700), key)
	first = expect(t, first, err, 200, "")
	for i := 0; i < 3; i++ {
		again, err := request("POST", "/credit", opBody(u, 700), key)
		again = expect(t, again, err, 200, "")
		if again.Balance != first.Balance {
			t.Fatalf("replay returned %d, want %d", again.Balance, first.Balance)
		}
	}
	expectBalance(t, u, 700)

	// Same key, different request.
	r, err := request("POST", "/credit", opBody(u, 701), key)
	expect(t, r, err, 422, "idempotency_key_reused")

	// Replayed failure stays a failure, and is not re-evaluated.
	dkey := map[string]string{"Idempotency-Key": "d-" + u}
	r, err = request("POST", "/debit", opBody(u, 1000), dkey)
	expect(t, r, err, 409, "insufficient_funds")
	credit(u, 1000)
	r, err = request("POST", "/debit", opBody(u, 1000), dkey)
	expect(t, r, err, 409, "insufficient_funds")
	expectBalance(t, u, 1700)
}

func TestLedgerRowsAreImmutable(t *testing.T) {
	pool := db(t)
	u := newUser("immut")
	r, err := credit(u, 123)
	expect(t, r, err, 200, "")

	ctx := t.Context()
	if _, err := pool.Exec(ctx, `UPDATE ledger SET amount = amount + 1 WHERE user_id = $1`, u); err == nil {
		t.Fatal("UPDATE on ledger succeeded")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM ledger WHERE user_id = $1`, u); err == nil {
		t.Fatal("DELETE on ledger succeeded")
	}
	var typ string
	var amount, after int64
	err = pool.QueryRow(ctx, `SELECT type::text, amount, balance_after FROM ledger WHERE user_id = $1`, u).Scan(&typ, &amount, &after)
	if err != nil || typ != "credit" || amount != 123 || after != 123 {
		t.Fatalf("ledger row: %v %s %d %d", err, typ, amount, after)
	}
}
