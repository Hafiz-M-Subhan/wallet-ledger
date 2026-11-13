// Command loadtest drives the API with concurrent clients and reports
// p50/p90/p99/max latency per endpoint.
//
// Phases:
//
//	mixed  N workers doing credit/debit/balance across the seeded users
//	hot    most workers hammer ONE user while the rest keep doing the mixed
//	       workload; "other" rows show whether the hot user hurts other users
//
// Usage (inside compose):
//
//	docker compose --profile tools run --rm loadtest -duration 60s
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type config struct {
	baseURL     string
	token       string
	workers     int
	hotWorkers  int
	duration    time.Duration
	hotDuration time.Duration
	warmup      time.Duration
	seedUsers   int
	whales      int
	phases      string
	out         string
}

func main() {
	var c config
	flag.StringVar(&c.baseURL, "base-url", envOr("BASE_URL", "http://localhost:8080"), "API base URL")
	flag.IntVar(&c.workers, "workers", 100, "concurrent client workers")
	flag.IntVar(&c.hotWorkers, "hot-workers", 80, "workers pinned to the hot user in the hot phase")
	flag.DurationVar(&c.duration, "duration", 60*time.Second, "mixed phase duration")
	flag.DurationVar(&c.hotDuration, "hot-duration", 30*time.Second, "hot phase duration")
	flag.DurationVar(&c.warmup, "warmup", 10*time.Second, "warm-up before measuring (not recorded)")
	flag.IntVar(&c.seedUsers, "seed-users", 100000, "number of seeded users (seed_0000001..)")
	flag.IntVar(&c.whales, "whales", 10, "number of whale users at the start of the seed range")
	flag.StringVar(&c.phases, "phases", "mixed,hot", "comma-separated phases to run")
	flag.StringVar(&c.out, "out", "", "also write the markdown report to this file")
	flag.Parse()
	c.token = os.Getenv("API_TOKEN")
	if c.token == "" {
		fmt.Fprintln(os.Stderr, "API_TOKEN must be set")
		os.Exit(2)
	}

	lt := &loadTester{cfg: c, client: &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        c.workers * 2,
			MaxIdleConnsPerHost: c.workers * 2,
			IdleConnTimeout:     60 * time.Second,
		},
	}}

	var report strings.Builder
	fmt.Fprintf(&report, "Load test: %d workers, base %s, %s\n\n", c.workers, c.baseURL, time.Now().UTC().Format(time.RFC3339))

	if c.warmup > 0 {
		fmt.Fprintf(os.Stderr, "warming up for %v...\n", c.warmup)
		lt.run(c.warmup, c.workers, 0, "")
	}
	for _, phase := range strings.Split(c.phases, ",") {
		switch strings.TrimSpace(phase) {
		case "mixed":
			fmt.Fprintf(os.Stderr, "mixed phase for %v...\n", c.duration)
			stats := lt.run(c.duration, c.workers, 0, "")
			fmt.Fprintf(&report, "### Mixed workload (%v, %d workers, 30%% credit / 30%% debit / 40%% balance)\n\n", c.duration, c.workers)
			report.WriteString(stats.table(c.duration))
		case "hot":
			hot := fmt.Sprintf("hot_%d", time.Now().UnixNano())
			if _, code, err := lt.do("POST", "/credit", body(hot, 1_000_000_000)); err != nil || code != 200 {
				fmt.Fprintf(os.Stderr, "could not create hot user: %v %d\n", err, code)
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "hot phase for %v...\n", c.hotDuration)
			stats := lt.run(c.hotDuration, c.workers, c.hotWorkers, hot)
			fmt.Fprintf(&report, "### Hot user (%v, %d workers on one user + %d workers on other users)\n\n", c.hotDuration, c.hotWorkers, c.workers-c.hotWorkers)
			report.WriteString(stats.table(c.hotDuration))
		default:
			fmt.Fprintf(os.Stderr, "unknown phase %q\n", phase)
			os.Exit(2)
		}
		report.WriteString("\n")
	}

	fmt.Print(report.String())
	if c.out != "" {
		if err := os.WriteFile(c.out, []byte(report.String()), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
}

type loadTester struct {
	cfg    config
	client *http.Client
}

type series struct {
	lat      []time.Duration
	byStatus map[int]int
	failures int // transport errors and statuses other than 200/404/409
}

type stats map[string]*series

func (s stats) add(name string, d time.Duration, status int, err error) {
	se := s[name]
	if se == nil {
		se = &series{byStatus: map[int]int{}}
		s[name] = se
	}
	se.lat = append(se.lat, d)
	if err != nil {
		se.failures++
		return
	}
	se.byStatus[status]++
	if status != 200 && status != 404 && status != 409 {
		se.failures++
	}
}

func (s stats) merge(o stats) {
	for name, src := range o {
		dst := s[name]
		if dst == nil {
			s[name] = src
			continue
		}
		dst.lat = append(dst.lat, src.lat...)
		dst.failures += src.failures
		for k, v := range src.byStatus {
			dst.byStatus[k] += v
		}
	}
}

// run starts `workers` goroutines for d. If hotUser is set, the first
// hotWorkers of them target only that user.
func (lt *loadTester) run(d time.Duration, workers, hotWorkers int, hotUser string) stats {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var mu sync.Mutex
	total := stats{}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			local := stats{}
			rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(w)))
			isHot := hotUser != "" && w < hotWorkers
			for ctx.Err() == nil {
				user, prefix := lt.pickUser(rng), ""
				if hotUser != "" {
					prefix = "other "
				}
				if isHot {
					user, prefix = hotUser, "hot "
				}
				var method, path, payload, name string
				switch p := rng.IntN(100); {
				case p < 30:
					method, path, payload, name = "POST", "/credit", body(user, 1+rng.Int64N(10_000)), "credit"
				case p < 60:
					method, path, payload, name = "POST", "/debit", body(user, 1+rng.Int64N(5_000)), "debit"
				default:
					method, path, name = "GET", "/balance/"+user, "balance"
				}
				t0 := time.Now()
				_, status, err := lt.do(method, path, payload)
				if ctx.Err() != nil && err != nil {
					break // cut off by the end of the phase, not a real failure
				}
				local.add(prefix+name, time.Since(t0), status, err)
			}
			mu.Lock()
			total.merge(local)
			mu.Unlock()
		}(w)
	}
	wg.Wait()
	return total
}

// pickUser: mostly uniform over the seeded users, 5% on the whales.
func (lt *loadTester) pickUser(rng *rand.Rand) string {
	var i int
	if lt.cfg.whales > 0 && rng.IntN(100) < 5 {
		i = 1 + rng.IntN(lt.cfg.whales)
	} else {
		i = 1 + rng.IntN(lt.cfg.seedUsers)
	}
	return fmt.Sprintf("seed_%07d", i)
}

func (lt *loadTester) do(method, path, payload string) ([]byte, int, error) {
	var rdr io.Reader
	if payload != "" {
		rdr = bytes.NewBufferString(payload)
	}
	req, err := http.NewRequest(method, lt.cfg.baseURL+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+lt.cfg.token)
	if payload != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := lt.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

func body(user string, amount int64) string {
	return `{"user_id":"` + user + `","amount":` + strconv.FormatInt(amount, 10) + `}`
}

func (s stats) table(d time.Duration) string {
	names := make([]string, 0, len(s))
	for n := range s {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("| endpoint | requests | req/s | p50 ms | p90 ms | p99 ms | max ms | 200 | 404 | 409 | failures |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, n := range names {
		se := s[n]
		sort.Slice(se.lat, func(i, j int) bool { return se.lat[i] < se.lat[j] })
		fmt.Fprintf(&b, "| %s | %d | %.0f | %s | %s | %s | %s | %d | %d | %d | %d |\n",
			n, len(se.lat), float64(len(se.lat))/d.Seconds(),
			ms(pct(se.lat, 0.50)), ms(pct(se.lat, 0.90)), ms(pct(se.lat, 0.99)), ms(pct(se.lat, 1)),
			se.byStatus[200], se.byStatus[404], se.byStatus[409], se.failures)
	}
	return b.String()
}

func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted))*p+0.999999) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func ms(d time.Duration) string { return fmt.Sprintf("%.2f", float64(d.Microseconds())/1000) }

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
