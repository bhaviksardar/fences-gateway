package sync

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	gosync "sync"
	"testing"
	"time"

	"github.com/bhaviksardar/fences-gateway/pkg/bundle"
	"github.com/bhaviksardar/fences-gateway/pkg/queue"
)

// platform is a fake Fences platform that checks every signature.
type platform struct {
	t           *testing.T
	signer      ed25519.PrivateKey
	mu          gosync.Mutex
	clientKey   ed25519.PublicKey
	syncs       []map[string]any
	stored      map[int64]bool
	status      int // forced reply status for /decisions and /sync, 0 for normal
	emptyReply  bool
	syncFails   int // how many syncs to answer 503 first
	commandAckd bool
	ignoreAcks  bool // re-send the command even once it is acked
}

func (p *platform) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.URL.Path == "/api/gateways/enroll" {
		var req struct {
			Token     string `json:"token"`
			PublicKey []byte `json:"public_key"`
		}
		json.Unmarshal(body, &req)
		if req.Token != "fge_ok" {
			w.WriteHeader(401)
			return
		}
		p.clientKey = req.PublicKey
		json.NewEncoder(w).Encode(map[string]any{"gateway_id": "gw_1", "sync_interval_s": 15,
			"signing_keys": []map[string]any{{"key_id": "k1", "public_key": []byte(p.signer.Public().(ed25519.PublicKey))}}})
		return
	}
	sum := sha256.Sum256(body)
	sig, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-Fences-Signature"))
	msg := "POST\n" + r.URL.Path + "\n" + r.Header.Get("X-Fences-Timestamp") + "\n" + hex.EncodeToString(sum[:])
	if r.Header.Get("X-Fences-Gateway-Id") != "gw_1" || !ed25519.Verify(p.clientKey, []byte(msg), sig) {
		w.WriteHeader(401)
		return
	}
	if p.status != 0 {
		w.WriteHeader(p.status)
		return
	}
	switch r.URL.Path {
	case "/api/gateways/sync":
		if p.syncFails > 0 {
			p.syncFails--
			w.WriteHeader(503)
			return
		}
		var req map[string]any
		json.Unmarshal(body, &req)
		p.syncs = append(p.syncs, req)
		for _, a := range req["acks"].([]any) {
			if p.ignoreAcks {
				break
			}
			p.commandAckd = p.commandAckd || a.(map[string]any)["id"] == "quarantine:bot:1"
		}
		reply := map[string]any{"commands": []any{}}
		if req["bundle_version"].(float64) < 7 {
			payload := `{"version":7,"gateway_id":"gw_1","agents":{},"policy":{"rules":[]}}`
			reply["bundle"] = bundle.Envelope{KeyID: "k1", Payload: base64.StdEncoding.EncodeToString([]byte(payload)),
				Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(p.signer, []byte(payload)))}
		}
		if !p.commandAckd { // re-sent until acknowledged
			reply["commands"] = []any{"junk", map[string]any{"type": "quarantine", "id": "quarantine:bot:1", "agent": "bot"}}
		}
		json.NewEncoder(w).Encode(reply)
	case "/api/gateways/decisions":
		if p.emptyReply {
			w.Write([]byte("{}"))
			return
		}
		var req struct {
			Events []struct{ Seq int64 } `json:"events"`
		}
		json.Unmarshal(body, &req)
		var top int64
		for _, e := range req.Events {
			p.stored[e.Seq] = true
			top = max(top, e.Seq)
		}
		json.NewEncoder(w).Encode(map[string]any{"acked_seq": top})
	}
}

func setup(t *testing.T) (*platform, *Client, *bundle.Store) {
	_, signer, _ := ed25519.GenerateKey(nil)
	p := &platform{t: t, signer: signer, stored: map[int64]bool{}}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, Kind: "gateway", Version: "0.1.0"}
	path := filepath.Join(t.TempDir(), "identity.json")
	if _, err := Enroll(context.Background(), c, "fge_bad", "gw", path); err != ErrRevoked {
		t.Fatalf("bad token: %v", err)
	}
	if _, err := Enroll(context.Background(), c, "fge_ok", "gw", path); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("identity mode %v, want 0600", info.Mode().Perm())
	}
	id, err := LoadIdentity(path)
	if err != nil || id.GatewayID != "gw_1" || !id.PrivateKey.Equal(c.ID.PrivateKey) {
		t.Fatalf("reload identity: %v", err)
	}
	keys := map[string]ed25519.PublicKey{}
	for k, v := range id.SigningKeys {
		keys[k] = v
	}
	return p, c, &bundle.Store{Path: filepath.Join(t.TempDir(), "bundle.json"), Keys: keys, GatewayID: "gw_1"}
}

func TestSync(t *testing.T) {
	p, c, store := setup(t)
	var applied []string
	s := &Syncer{Client: c, Store: store, Apply: func(c Command) { applied = append(applied, c.ID) },
		Health: func() map[string]any { return map[string]any{"queue_depth": 3} }}
	ctx := context.Background()

	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if store.Current() == nil || store.Current().Version != 7 || len(applied) != 1 {
		t.Fatalf("first sync: bundle %v, applied %v", store.Current(), applied)
	}
	p.mu.Lock()
	p.commandAckd, p.ignoreAcks = false, true // the platform didn't get the ack, so it re-sends the command
	p.mu.Unlock()
	s.Once(ctx)
	s.Once(ctx)
	p.mu.Lock()
	p.ignoreAcks = false
	p.mu.Unlock()
	s.Once(ctx)
	if len(applied) != 1 || !p.commandAckd {
		t.Fatalf("a re-sent command is applied once and acked again: applied %v, acked %v", applied, p.commandAckd)
	}
	s.Ack(Ack{ID: "quarantine:bot:1", Type: "quarantine", Extra: map[string]any{"containment": "ok"}})
	s.Once(ctx)
	last := p.syncs[len(p.syncs)-1]
	if last["bundle_version"] != 7.0 || last["health"].(map[string]any)["queue_depth"] != 3.0 ||
		last["acks"].([]any)[0].(map[string]any)["containment"] != "ok" {
		t.Fatalf("sync request: %v", last)
	}

	// Failures back off, and a revoked key stops the loop from syncing
	firstBackoff, openCircuit, revokedLog = time.Millisecond, 5*time.Millisecond, time.Hour
	p.mu.Lock()
	p.syncFails = 3
	n := len(p.syncs)
	p.mu.Unlock()
	ctx2, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { s.Run(ctx2); close(done) }()
	defer func() { cancel(); <-done }() // before other tests change the backoff variables
	time.Sleep(100 * time.Millisecond)
	p.mu.Lock()
	recovered := len(p.syncs) > n
	p.status = 401
	p.mu.Unlock()
	if !recovered {
		t.Error("sync didn't recover after 3 failures")
	}
	s.Wake()
	time.Sleep(50 * time.Millisecond)
	if store.Current().Version != 7 {
		t.Error("a revoked key must keep the last good bundle")
	}
}

func TestBackoff(t *testing.T) {
	firstBackoff, maxBackoff, openCircuit = time.Second, 5*time.Minute, time.Minute
	for f, top := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 4: 8 * time.Second} {
		for i := 0; i < 100; i++ {
			if d := backoff(f); d <= 0 || d > top {
				t.Fatalf("backoff(%d) = %v, want in (0, %v]", f, d, top)
			}
		}
	}
	if backoff(5) != time.Minute || backoff(50) != time.Minute {
		t.Error("after 5 failures the circuit opens: once a minute")
	}
}

func TestShip(t *testing.T) {
	p, c, _ := setup(t)
	q, _ := queue.Open(filepath.Join(t.TempDir(), "q.db"), 0)
	defer q.Close()
	for i := 0; i < 120; i++ { // over 100: sent without waiting 2 s
		q.Push(map[string]any{"decision": "allow"})
	}
	if err := shipOnce(context.Background(), c, q, nil); err != nil || q.Depth() != 0 || len(p.stored) != 120 {
		t.Fatalf("ship: %v, depth %d, stored %d", err, q.Depth(), len(p.stored))
	}
	for _, status := range []int{409, 403} { // a repeat, and a batch for an agent this gateway doesn't serve: both leave the queue
		q.Push(map[string]any{})
		p.status = status
		if err := shipOnce(context.Background(), c, q, slogDiscard()); err != nil || q.Depth() != 0 {
			t.Errorf("%d: %v, depth %d", status, err, q.Depth())
		}
	}
	q.Push(map[string]any{})
	p.status = 500
	if shipOnce(context.Background(), c, q, nil) == nil || q.Depth() != 1 {
		t.Error("a 500 must keep the events queued")
	}
	p.status, p.emptyReply = 0, true
	if shipOnce(context.Background(), c, q, nil) == nil || q.Depth() != 1 {
		t.Error("a reply without acked_seq must keep the events queued")
	}
}

func TestNoRedirects(t *testing.T) {
	_, c, _ := setup(t)
	target := c.BaseURL
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	c.BaseURL = redirect.URL
	if err := c.Post(context.Background(), "/api/gateways/sync", map[string]any{"bundle_version": 7, "acks": []any{}}, nil); err == nil {
		t.Error("a redirect must not be followed: it would carry the signed request elsewhere")
	}
}

func TestCheckURL(t *testing.T) {
	for u, ok := range map[string]bool{
		"https://fences.example": true, "http://localhost:8000": true, "http://127.0.0.1:1": true, "http://[::1]:1": true,
		"http://fences.example": false, "https://user:pw@fences.example": false, "fences.example": false, "ftp://x": false,
	} {
		if (CheckURL(u) == nil) != ok {
			t.Errorf("CheckURL(%q): want ok=%v", u, ok)
		}
	}
}

func slogDiscard() *slog.Logger { return slog.New(slog.DiscardHandler) }
