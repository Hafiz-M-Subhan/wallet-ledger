package ledger

import (
	"context"
	"sync"
	"sync/atomic"
)

const shards = 64

// pending is one credit/debit request waiting for its user's queue.
type pending struct {
	op     Op
	amount int64
	key    string // Idempotency-Key, "" if none
	state  atomic.Int32
	done   chan outcome // buffered(1); receives exactly one outcome once taken
}

const (
	stateQueued int32 = iota
	stateTaken
	stateAbandoned
)

type outcome struct {
	balance int64
	err     error
}

type userQueue struct {
	items  []*pending
	active bool // a drain goroutine is running for this user
}

// combiner keeps one FIFO queue per user with in-flight writes, and at most
// one goroutine ("drainer") per user executing them.
//
// Why: all writes for one user serialise on that user's balances row lock
// anyway, so each one pays a full commit (WAL fsync) back to back. With a
// queue, when several requests for the same user are waiting, the drainer
// takes them all and executes them in ONE transaction, in arrival order, so
// N writes cost one fsync instead of N. Each request still gets its own
// ledger row and the exact balance after its own operation.
//
// It also keeps the connection pool fair: a hot user uses at most one
// connection per API instance, while its backlog waits as goroutines, so
// other users are never starved of connections.
//
// When a user has no contention, the batch is a single request and runs as
// one autocommitted statement — no extra latency.
//
// Correctness never depends on this type: the database row lock and
// single-transaction writes make the result correct even with several API
// instances (each with its own combiner).
type combiner struct {
	shards   [shards]combShard
	maxBatch int
	exec     func(userID string, batch []*pending) []outcome
}

type combShard struct {
	mu sync.Mutex
	m  map[string]*userQueue
}

func newCombiner(maxBatch int, exec func(string, []*pending) []outcome) *combiner {
	c := &combiner{maxBatch: maxBatch, exec: exec}
	for i := range c.shards {
		c.shards[i].m = make(map[string]*userQueue)
	}
	return c
}

// submit enqueues p and waits for its outcome. If ctx ends while p is still
// queued, p is abandoned (never executed) and ctx.Err() is returned. Once
// taken for execution, it runs to completion regardless of ctx.
func (c *combiner) submit(ctx context.Context, userID string, p *pending) (int64, error) {
	sh := &c.shards[fnv32a(userID)%shards]

	sh.mu.Lock()
	q := sh.m[userID]
	if q == nil {
		q = &userQueue{}
		sh.m[userID] = q
	}
	q.items = append(q.items, p)
	start := !q.active
	q.active = true
	sh.mu.Unlock()

	if start {
		go c.drain(sh, userID, q)
	}

	select {
	case o := <-p.done:
		return o.balance, o.err
	case <-ctx.Done():
		if p.state.CompareAndSwap(stateQueued, stateAbandoned) {
			return 0, ctx.Err()
		}
		o := <-p.done // already executing; report the real outcome
		return o.balance, o.err
	}
}

func (c *combiner) drain(sh *combShard, userID string, q *userQueue) {
	for {
		sh.mu.Lock()
		batch := c.take(q)
		if len(batch) == 0 {
			// take only returns empty when the queue is empty.
			q.active = false
			delete(sh.m, userID)
			sh.mu.Unlock()
			return
		}
		sh.mu.Unlock()

		outs := c.exec(userID, batch)
		for i, p := range batch {
			p.done <- outs[i]
		}
	}
}

// take removes the next batch from the head of q: either a run of up to
// maxBatch plain requests, or a single request carrying an Idempotency-Key
// (those run in their own transaction). Abandoned requests are dropped.
// Arrival order is preserved. Caller holds the shard lock.
func (c *combiner) take(q *userQueue) []*pending {
	var batch []*pending
	i := 0
	for i < len(q.items) && len(batch) < c.maxBatch {
		p := q.items[i]
		if p.key != "" && len(batch) > 0 {
			break // idempotent request starts the next batch
		}
		i++
		if !p.state.CompareAndSwap(stateQueued, stateTaken) {
			continue // abandoned by its client
		}
		batch = append(batch, p)
		if p.key != "" {
			break
		}
	}
	if i == len(q.items) {
		q.items = nil
	} else {
		q.items = q.items[i:]
	}
	return batch
}

// size reports the number of users with queued or running writes (tests).
func (c *combiner) size() int {
	n := 0
	for i := range c.shards {
		c.shards[i].mu.Lock()
		n += len(c.shards[i].m)
		c.shards[i].mu.Unlock()
	}
	return n
}

func fnv32a(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}
