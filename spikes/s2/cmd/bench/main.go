// S2 overhead and cancel measurements against the mock backend.
//
//	go run ./cmd/bench latency -n 2000   # added latency and time-to-first-token, interleaved
//	go run ./cmd/bench cancel -n 30      # how fast does a client cancel reach the backend
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

var (
	direct = flag.String("direct", "http://127.0.0.1:11436", "mock backend")
	proxy  = flag.String("proxy", "http://127.0.0.1:11435", "proxy")
	n      = flag.Int("n", 2000, "requests per target")
	httpc  = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 8, DisableCompression: true}}
)

type target struct {
	name, base, model string
}

func pct(xs []float64, p float64) float64 {
	sort.Float64s(xs)
	return xs[int(float64(len(xs)-1)*p)]
}

func post(ctx context.Context, base, id string, body map[string]any) (*http.Response, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Req-Id", id)
	return httpc.Do(req)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: bench latency|cancel [-n N]")
		os.Exit(2)
	}
	cmd := os.Args[1]
	flag.CommandLine.Parse(os.Args[2:])
	ts := []target{{"direct (mock)", *direct, "mock-1"}, {"proxy passthrough", *proxy, "mock-1"}, {"proxy agent (noskill/auto)", *proxy, "noskill/auto"}}
	switch cmd {
	case "latency":
		latency(ts)
	case "cancel":
		cancel(ts)
	}
}

func latency(ts []target) {
	type res struct{ total, ttft, nonstream []float64 }
	rs := make([]res, len(ts))
	warm := *n / 10
	for i := -warm; i < *n; i++ {
		for k, t := range ts {
			// non-stream
			t0 := time.Now()
			resp, err := post(context.Background(), t.base, "", map[string]any{"model": t.model, "stream": false, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
			if err != nil {
				panic(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			ns := float64(time.Since(t0).Microseconds()) / 1000
			// stream
			t0 = time.Now()
			resp, err = post(context.Background(), t.base, "", map[string]any{"model": t.model, "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
			if err != nil {
				panic(err)
			}
			sc := bufio.NewScanner(resp.Body)
			var first float64
			for sc.Scan() {
				l := sc.Text()
				if first == 0 && strings.Contains(l, `"content":"tok`) {
					first = float64(time.Since(t0).Microseconds()) / 1000
				}
			}
			resp.Body.Close()
			total := float64(time.Since(t0).Microseconds()) / 1000
			if i >= 0 {
				rs[k].nonstream = append(rs[k].nonstream, ns)
				rs[k].ttft = append(rs[k].ttft, first)
				rs[k].total = append(rs[k].total, total)
			}
		}
	}
	fmt.Printf("%d requests per target (interleaved), mock streams 40 tokens with no delay\n\n", *n)
	fmt.Println("| Target | Non-stream p50 / p99 ms | First content token p50 / p99 ms | Stream total p50 / p99 ms |\n|---|---|---|---|")
	for k, t := range ts {
		fmt.Printf("| %s | %.3f / %.3f | %.3f / %.3f | %.3f / %.3f |\n", t.name,
			pct(rs[k].nonstream, .5), pct(rs[k].nonstream, .99), pct(rs[k].ttft, .5), pct(rs[k].ttft, .99), pct(rs[k].total, .5), pct(rs[k].total, .99))
	}
	fmt.Println("\nAdded by the proxy (vs direct):\n\n| Target | Non-stream p50 / p99 ms | First content token p50 / p99 ms |\n|---|---|---|")
	for k := 1; k < len(ts); k++ {
		fmt.Printf("| %s | %+.3f / %+.3f | %+.3f / %+.3f |\n", ts[k].name,
			pct(rs[k].nonstream, .5)-pct(rs[0].nonstream, .5), pct(rs[k].nonstream, .99)-pct(rs[0].nonstream, .99),
			pct(rs[k].ttft, .5)-pct(rs[0].ttft, .5), pct(rs[k].ttft, .99)-pct(rs[0].ttft, .99))
	}
}

func cancel(ts []target) {
	fmt.Println("| Target | Runs | Median ms from client close to backend noticing | Max ms | Tokens the backend had sent |\n|---|---|---|---|---|")
	for _, t := range ts {
		var ds []float64
		var toks []float64
		for i := 0; i < *n; i++ {
			id := fmt.Sprintf("cancel-%s-%d-%d", strings.Fields(t.name)[0], time.Now().UnixNano(), i)
			ctx, stop := context.WithCancel(context.Background())
			resp, err := post(ctx, t.base, id, map[string]any{"model": t.model, "stream": true, "messages": []any{map[string]any{"role": "user", "content": "slow long answer"}}})
			if err != nil {
				panic(err)
			}
			sc := bufio.NewScanner(resp.Body)
			for k := 0; k < 12 && sc.Scan(); k++ {
			}
			closed := time.Now().UnixNano()
			stop()
			resp.Body.Close()
			var got struct {
				CancelledAt int64 `json:"cancelled_at"`
				Tokens      int   `json:"tokens"`
			}
			for w := 0; w < 100; w++ { // poll up to 2 s
				r, err := http.Get(*direct + "/_cancel/" + id)
				if err == nil {
					json.NewDecoder(r.Body).Decode(&got)
					r.Body.Close()
				}
				if got.CancelledAt != 0 {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if got.CancelledAt == 0 {
				ds = append(ds, 2000)
			} else {
				ds = append(ds, float64(got.CancelledAt-closed)/1e6)
			}
			toks = append(toks, float64(got.Tokens))
		}
		fmt.Printf("| %s | %d | %.0f | %.0f | median %.0f |\n", t.name, len(ds), pct(append([]float64{}, ds...), .5), pct(append([]float64{}, ds...), 1), pct(toks, .5))
	}
}
