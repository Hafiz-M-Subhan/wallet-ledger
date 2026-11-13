package e2e

import (
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Rule 1: two concurrent debits of 60 against 100 give exactly one 200 and
// one 409. Repeated on fresh users to give the race many chances to show.
func TestConcurrentDebitsNeverOverdraw(t *testing.T) {
	const rounds = 100
	for i := 0; i < rounds; i++ {
		u := newUser("dd")
		r, err := credit(u, 100)
		expect(t, r, err, 200, "")

		var wg sync.WaitGroup
		results := make([]result, 2)
		errs := make([]error, 2)
		start := make(chan struct{})
		for j := 0; j < 2; j++ {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				<-start
				results[j], errs[j] = debit(u, 60)
			}(j)
		}
		close(start)
		wg.Wait()

		ok, conflict := 0, 0
		for j := 0; j < 2; j++ {
			if errs[j] != nil {
				t.Fatalf("round %d: %v", i, errs[j])
			}
			switch {
			case results[j].Status == 200 && results[j].Balance == 40:
				ok++
			case results[j].Status == 409 && results[j].Error == "insufficient_funds":
				conflict++
			default:
				t.Fatalf("round %d: unexpected response %d %s", i, results[j].Status, results[j].Raw)
			}
		}
		if ok != 1 || conflict != 1 {
			t.Fatalf("round %d: want one 200 and one 409, got %d and %d", i, ok, conflict)
		}
		expectBalance(t, u, 40)
	}
}

// Many debits racing to drain one balance: exactly floor(1000/15) succeed.
func TestConcurrentDrain(t *testing.T) {
	u := newUser("drain")
	r, err := credit(u, 1000)
	expect(t, r, err, 200, "")

	const n = 200
	var ok, conflict, other atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, err := debit(u, 15)
			switch {
			case err != nil:
				other.Add(1)
			case r.Status == 200 && r.Balance >= 0:
				ok.Add(1)
			case r.Status == 409:
				conflict.Add(1)
			default:
				other.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if ok.Load() != 66 || conflict.Load() != n-66 || other.Load() != 0 {
		t.Fatalf("want 66 ok / %d conflicts, got %d / %d (other %d)", n-66, ok.Load(), conflict.Load(), other.Load())
	}
	expectBalance(t, u, 10)
}

// Rule 2: 1,000 concurrent credits of 100 to one user raise the balance by
// exactly 100,000. Every returned balance is distinct (each credit saw the
// previous one), which also proves the returned value is "immediately after
// that operation".
func TestNoLostUpdates(t *testing.T) {
	u := newUser("lost")
	r, err := credit(u, 5)
	expect(t, r, err, 200, "")

	const n = 1000
	var wg sync.WaitGroup
	start := make(chan struct{})
	balances := make([]int64, n)
	failures := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r, err := credit(u, 100)
			if err != nil || r.Status != 200 {
				failures <- fmt.Sprintf("%v %d %s", err, r.Status, r.Raw)
				return
			}
			balances[i] = r.Balance
		}(i)
	}
	close(start)
	wg.Wait()
	close(failures)
	for f := range failures {
		t.Fatalf("credit failed: %s", f)
	}

	expectBalance(t, u, 5+n*100)
	seen := make(map[int64]bool, n)
	for _, b := range balances {
		if seen[b] || (b-5)%100 != 0 || b < 105 || b > 5+n*100 {
			t.Fatalf("returned balance %d is duplicated or impossible", b)
		}
		seen[b] = true
	}

	if os.Getenv("DATABASE_URL") != "" {
		pool := db(t)
		var rows int64
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM ledger WHERE user_id = $1`, u).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != n+1 {
			t.Fatalf("want %d ledger rows, got %d", n+1, rows)
		}
	}
}

// Random credits and debits from many goroutines on a few users. Tracks what
// the API acknowledged and checks the final balances against it.
func TestMixedRaceMatchesAcknowledgedOps(t *testing.T) {
	users := []string{newUser("mix"), newUser("mix"), newUser("mix"), newUser("mix")}
	expected := make([]atomic.Int64, len(users))
	for i, u := range users {
		r, err := credit(u, 500)
		expect(t, r, err, 200, "")
		expected[i].Store(500)
	}

	const workers, opsPerWorker = 64, 40
	var wg sync.WaitGroup
	var unexpected atomic.Int64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < opsPerWorker; k++ {
				i := rand.IntN(len(users))
				amt := randAmount(300)
				if rand.IntN(2) == 0 {
					r, err := credit(users[i], amt)
					if err == nil && r.Status == 200 {
						expected[i].Add(amt)
					} else {
						unexpected.Add(1)
					}
				} else {
					r, err := debit(users[i], amt)
					switch {
					case err == nil && r.Status == 200:
						if r.Balance < 0 {
							unexpected.Add(1)
						}
						expected[i].Add(-amt)
					case err == nil && r.Status == 409:
					default:
						unexpected.Add(1)
					}
				}
			}
		}()
	}
	wg.Wait()
	if unexpected.Load() != 0 {
		t.Fatalf("%d unexpected responses", unexpected.Load())
	}
	for i, u := range users {
		expectBalance(t, u, expected[i].Load())
	}
}

// Rule 4: once credit/debit returns 200, a later read reflects it. Balances
// are only ever credited here, so a read must never be below a balance some
// earlier write already returned.
func TestReadYourWrites(t *testing.T) {
	u := newUser("ryw")
	var wg sync.WaitGroup
	var violations atomic.Int64
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 20; k++ {
				w, err := credit(u, 10)
				if err != nil || w.Status != 200 {
					violations.Add(1)
					continue
				}
				r, err := balance(u)
				if err != nil || r.Status != 200 || r.Balance < w.Balance {
					violations.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if violations.Load() != 0 {
		t.Fatalf("%d read-your-writes violations", violations.Load())
	}
	expectBalance(t, u, 32*20*10)
}

// Rule 5: heavy traffic on one user must not slow down other users.
// Measures another user's latency alone, then while 96 goroutines hammer a
// hot user, and compares.
func TestHotUserDoesNotBlockOthers(t *testing.T) {
	hot, cold := newUser("hot"), newUser("cold")
	r, err := credit(hot, 1_000_000)
	expect(t, r, err, 200, "")
	r, err = credit(cold, 1_000_000)
	expect(t, r, err, 200, "")

	measure := func(n int) []time.Duration {
		out := make([]time.Duration, 0, n)
		for i := 0; i < n; i++ {
			t0 := time.Now()
			var r result
			var err error
			if i%2 == 0 {
				r, err = credit(cold, 1)
			} else {
				r, err = balance(cold)
			}
			if err != nil || r.Status != 200 {
				t.Errorf("cold user request failed: %v %d %s", err, r.Status, r.Raw)
			}
			out = append(out, time.Since(t0))
		}
		return out
	}

	base := measure(200)

	stop := make(chan struct{})
	var hotOK, hotFail atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < 96; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				var r result
				var err error
				switch w % 3 {
				case 0:
					r, err = credit(hot, 7)
				case 1:
					r, err = debit(hot, 5)
				default:
					r, err = balance(hot)
				}
				if err == nil && (r.Status == 200 || r.Status == 409) {
					hotOK.Add(1)
				} else {
					hotFail.Add(1)
				}
			}
		}(w)
	}
	time.Sleep(300 * time.Millisecond) // let the hot queue build up
	loaded := measure(200)
	close(stop)
	wg.Wait()

	bp50, bp99 := percentile(base, 0.5), percentile(base, 0.99)
	lp50, lp99 := percentile(loaded, 0.5), percentile(loaded, 0.99)
	t.Logf("cold user alone:          p50=%v p99=%v", bp50, bp99)
	t.Logf("cold user with hot load:  p50=%v p99=%v (hot user served %d requests, %d failed)", lp50, lp99, hotOK.Load(), hotFail.Load())

	if hotFail.Load() != 0 {
		t.Fatalf("%d hot-user requests failed", hotFail.Load())
	}
	// Generous bound: the hot user adds CPU load on a shared laptop, but must
	// not make other users wait behind its lock queue (which would show up as
	// latencies in the hundreds of milliseconds or more).
	if lp99 > 250*time.Millisecond {
		t.Fatalf("cold user p99 under hot load is %v", lp99)
	}
}
