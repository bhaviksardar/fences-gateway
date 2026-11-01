package proxy

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/bhaviksardar/fences-gateway/internal/engine"
	"github.com/bhaviksardar/fences-gateway/pkg/bundle"
	"github.com/bhaviksardar/fences-gateway/pkg/queue"
)

// TestLoad is the roadmap's load check: 1,000 decisions a second through one gateway, p99 added latency under 5 ms
// for decisions made locally. A short run by default; FENCES_LOAD_S=60 for a longer one (docs/load-test.md).
// The policy has an hour-long limit, so the history a limit counts grows the whole run.
func TestLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("load test")
	}
	seconds := 3
	if s, err := strconv.Atoi(os.Getenv("FENCES_LOAD_S")); err == nil && s > 0 {
		seconds = s
	}
	const rate = 1000
	sum := sha256.Sum256([]byte("tok"))
	payload := `{"version": 1, "gateway_id": "gw_1", "policy": {"rules": [
		{"id": "orders", "on": "http", "effect": "allow", "where": [{"field": "path", "op": "matches", "value": "/orders/*"}],
		 "limit": {"window_s": 3600, "max": 100000000, "sum": "body.amount"}},
		{"id": "no-admin", "on": "http", "effect": "deny", "where": [{"field": "path", "op": "matches", "value": "/admin*"}]}]},
		"agents": {"bot": {"credentials": [{"type": "token", "sha256": "` + hex.EncodeToString(sum[:]) + `"}], "upstreams": ["api"]}}}`
	pub, priv, _ := ed25519.GenerateKey(nil)
	store := &bundle.Store{Path: filepath.Join(t.TempDir(), "b.json"), Keys: map[string]ed25519.PublicKey{"k1": pub}, GatewayID: "gw_1"}
	env, _ := json.Marshal(bundle.Envelope{KeyID: "k1", Payload: base64.StdEncoding.EncodeToString([]byte(payload)),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(payload)))})
	if err := store.Apply(env); err != nil {
		t.Fatal(err)
	}
	q, _ := queue.Open(filepath.Join(t.TempDir(), "q.db"), 0)
	defer q.Close()
	eng := &engine.Engine{Store: store, Queue: q, Version: "load"}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte(`{"ok": true}`))
	}))
	defer api.Close()
	gw := httptest.NewServer((&Proxy{Engine: eng, APIs: map[string]API{"api": {URL: api.URL}}}).Handler())
	defer gw.Close()

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 512}}
	send := func(url string) (time.Duration, int) {
		req, _ := http.NewRequest("POST", url, strings.NewReader(`{"amount": 3}`))
		req.Header.Set("Authorization", "Bearer tok")
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			return 0, 0
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return time.Since(start), resp.StatusCode
	}
	// Open loop: a request every 1/rate seconds, whether or not the last ones have answered
	run := func(url string) ([]time.Duration, int) {
		var mu gosync.Mutex
		var took []time.Duration
		failed := 0
		var wg gosync.WaitGroup
		tick := time.NewTicker(time.Second / rate)
		defer tick.Stop()
		for i := 0; i < rate*seconds; i++ {
			<-tick.C
			wg.Add(1)
			go func() {
				defer wg.Done()
				d, code := send(url)
				mu.Lock()
				defer mu.Unlock()
				if code != 200 {
					failed++
					return
				}
				took = append(took, d)
			}()
		}
		wg.Wait()
		slices.Sort(took)
		return took, failed
	}
	p := func(d []time.Duration, q int) time.Duration { return d[len(d)*q/100] }

	direct, _ := run(api.URL + "/orders/1")
	eng.Health() // start the decisions-per-second count here
	start := time.Now()
	via, failed := run(gw.URL + "/http/api/orders/1")
	elapsed := time.Since(start)
	h := eng.Health()
	t.Logf("%d requests in %.1fs (%.0f/s), %d failed; decisions_per_s %v", len(via), elapsed.Seconds(), float64(len(via))/elapsed.Seconds(), failed, h["decisions_per_s"])
	t.Logf("client p50/p99: direct %v / %v, through the gateway %v / %v", p(direct, 50), p(direct, 99), p(via, 50), p(via, 99))
	t.Logf("engine p99 added latency (policy, limits, bookkeeping): %v ms; events queued %d", h["p99_added_ms"], q.Depth())
	if failed > 0 || len(via) != rate*seconds {
		t.Errorf("%d requests failed", failed)
	}
	if added := h["p99_added_ms"].(float64); added >= 5 {
		t.Errorf("p99 added latency %.2f ms, want under 5 ms", added)
	}
	// Client-side timing is the machine's as much as the gateway's: checked on a long run, logged on CI's short one
	if extra := p(via, 99) - p(direct, 99); os.Getenv("FENCES_LOAD_S") != "" && extra >= 5*time.Millisecond {
		t.Errorf("p99 through the gateway is %v over direct, want under 5 ms", extra)
	}
}
