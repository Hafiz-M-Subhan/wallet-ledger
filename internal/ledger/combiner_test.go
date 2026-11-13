package ledger

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeExec simulates a single balance per user with the real semantics
// (credits add, debits fail if insufficient) and records batch shapes.
type fakeExec struct {
	mu        sync.Mutex
	balances  map[string]int64
	inFlight  map[string]int
	maxFlight int
	batches   []int
	delay     time.Duration
}

func newFakeExec(delay time.Duration) *fakeExec {
	return &fakeExec{balances: map[string]int64{}, inFlight: map[string]int{}, delay: delay}
}

func (f *fakeExec) exec(user string, batch []*pending) []outcome {
	f.mu.Lock()
	f.inFlight[user]++
	if f.inFlight[user] > f.maxFlight {
		f.maxFlight = f.inFlight[user]
	}
	f.batches = append(f.batches, len(batch))
	f.mu.Unlock()

	time.Sleep(f.delay) // one "commit" per batch

	f.mu.Lock()
	defer f.mu.Unlock()
	outs := make([]outcome, len(batch))
	for i, p := range batch {
		if len(batch) > 1 && p.key != "" {
			panic("idempotent request batched with others")
		}
		switch p.op {
		case OpCredit:
			f.balances[user] += p.amount
			outs[i] = outcome{balance: f.balances[user]}
		case OpDebit:
			if f.balances[user] < p.amount {
				outs[i] = outcome{balance: f.balances[user], err: ErrInsufficientFunds}
			} else {
				f.balances[user] -= p.amount
				outs[i] = outcome{balance: f.balances[user]}
			}
		}
	}
	f.inFlight[user]--
	return outs
}

func newPending(op Op, amount int64, key string) *pending {
	return &pending{op: op, amount: amount, key: key, done: make(chan outcome, 1)}
}

func TestCombinerBatchesHotUserAndKeepsOneExecutorPerUser(t *testing.T) {
	f := newFakeExec(2 * time.Millisecond)
	c := newCombiner(64, f.exec)

	const n = 500
	var wg sync.WaitGroup
	seen := make([]int64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			bal, err := c.submit(context.Background(), "hot", newPending(OpCredit, 1, ""))
			if err != nil {
				t.Error(err)
			}
			seen[i] = bal
		}(i)
	}
	wg.Wait()

	if f.balances["hot"] != n {
		t.Fatalf("balance %d, want %d", f.balances["hot"], n)
	}
	if f.maxFlight != 1 {
		t.Fatalf("saw %d concurrent executions for one user", f.maxFlight)
	}
	uniq := map[int64]bool{}
	for _, b := range seen {
		uniq[b] = true
	}
	if len(uniq) != n {
		t.Fatalf("returned balances not distinct: %d unique of %d", len(uniq), n)
	}
	if len(f.batches) >= n {
		t.Fatalf("no batching happened: %d executions for %d requests", len(f.batches), n)
	}
	for _, b := range f.batches {
		if b > 64 {
			t.Fatalf("batch of %d exceeds max", b)
		}
	}
	if c.size() != 0 {
		t.Fatalf("queue table not cleaned up: %d entries", c.size())
	}
	t.Logf("%d requests in %d transactions", n, len(f.batches))
}

func TestCombinerDebitsNeverOverdraw(t *testing.T) {
	f := newFakeExec(time.Millisecond)
	c := newCombiner(64, f.exec)
	if _, err := c.submit(context.Background(), "u", newPending(OpCredit, 1000, "")); err != nil {
		t.Fatal(err)
	}
	var ok, conflict atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.submit(context.Background(), "u", newPending(OpDebit, 15, ""))
			if err == nil {
				ok.Add(1)
			} else {
				conflict.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 66 || f.balances["u"] != 10 {
		t.Fatalf("ok=%d balance=%d", ok.Load(), f.balances["u"])
	}
}

func TestCombinerUsersAreIndependent(t *testing.T) {
	block := make(chan struct{})
	c := newCombiner(64, func(user string, batch []*pending) []outcome {
		if user == "slow" {
			<-block
		}
		return make([]outcome, len(batch))
	})
	go c.submit(context.Background(), "slow", newPending(OpCredit, 1, ""))
	time.Sleep(10 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		c.submit(context.Background(), "fast", newPending(OpCredit, 1, ""))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("user fast was blocked by user slow")
	}
	close(block)
}

func TestCombinerIdempotentRequestsRunAlone(t *testing.T) {
	f := newFakeExec(time.Millisecond)
	c := newCombiner(64, f.exec) // fakeExec panics if a keyed request is batched
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := ""
			if i%3 == 0 {
				key = fmt.Sprintf("k%d", i)
			}
			c.submit(context.Background(), "u", newPending(OpCredit, 1, key))
		}(i)
	}
	wg.Wait()
	if f.balances["u"] != 100 {
		t.Fatalf("balance %d", f.balances["u"])
	}
}

func TestCombinerAbandonedRequestIsNeverExecuted(t *testing.T) {
	release := make(chan struct{})
	var executed atomic.Int64
	c := newCombiner(64, func(user string, batch []*pending) []outcome {
		<-release
		executed.Add(int64(len(batch)))
		return make([]outcome, len(batch))
	})
	go c.submit(context.Background(), "u", newPending(OpCredit, 1, "")) // occupies the drainer
	time.Sleep(10 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.submit(ctx, "u", newPending(OpCredit, 1, "")); err == nil {
		t.Fatal("expected context error")
	}
	close(release)
	time.Sleep(20 * time.Millisecond)
	if executed.Load() != 1 {
		t.Fatalf("executed %d requests, want 1 (abandoned one must be skipped)", executed.Load())
	}
	if c.size() != 0 {
		t.Fatalf("queue not cleaned up")
	}
}
