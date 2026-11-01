package k8s

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/bhaviksardar/fences-gateway/internal/approvals"
	"github.com/bhaviksardar/fences-gateway/internal/engine"
	"github.com/bhaviksardar/fences-gateway/pkg/bundle"
	"github.com/bhaviksardar/fences-gateway/pkg/queue"
	"github.com/bhaviksardar/fences-gateway/pkg/sync"
)

const sreBot = "system:serviceaccount:ops:sre-bot"

// The roadmap's AI SRE policy (spec/conformance/k8s.json)
const srePolicy = `{"rules": [
	{"id": "restart-pods", "on": "k8s", "effect": "allow", "where": [{"field": "verb", "op": "is", "value": "delete"}, {"field": "kind", "op": "is", "value": "Pod"}],
	 "limit": {"window_s": 600, "max": 3, "per": ["namespace"]}, "reason": "At most 3 pod restarts per namespace every 10 minutes"},
	{"id": "rotate-creds", "on": "k8s", "effect": "approve", "where": [{"field": "kind", "op": "is", "value": "Secret"}, {"field": "verb", "op": "in", "value": ["update", "patch"]},
	 {"field": "name", "op": "matches", "value": "*-credentials"}], "reason": "Credential rotation needs approval"}
]}`

type harness struct {
	engine *engine.Engine
	queue  *queue.Queue
	store  *bundle.Store
	priv   ed25519.PrivateKey
	mu     gosync.Mutex
	asked  []map[string]any
}

func setup(t *testing.T) *harness {
	h := &harness{}
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		h.mu.Lock()
		h.asked = append(h.asked, body)
		h.mu.Unlock()
		w.Write([]byte(`{"ok": true}`))
	}))
	t.Cleanup(platform.Close)
	_, gwKey, _ := ed25519.GenerateKey(nil)
	client := &sync.Client{BaseURL: platform.URL, Kind: "gateway", Version: "test", ID: &sync.Identity{GatewayID: "gw_1", PrivateKey: gwKey}}
	pub, priv, _ := ed25519.GenerateKey(nil)
	h.priv = priv
	h.store = &bundle.Store{Path: filepath.Join(t.TempDir(), "b.json"), Keys: map[string]ed25519.PublicKey{"k1": pub}, GatewayID: "gw_1"}
	h.bundle(t, 1, "[]")
	h.queue, _ = queue.Open(filepath.Join(t.TempDir(), "q.db"), 0)
	t.Cleanup(func() { h.queue.Close() })
	h.engine = &engine.Engine{Store: h.store, Queue: h.queue, Approvals: &approvals.Manager{Client: client}, Version: "test"}
	return h
}

func (h *harness) bundle(t *testing.T, version int, quarantined string) {
	pol := strings.Replace(srePolicy, `{"rules"`, `{"quarantined": `+quarantined+`, "rules"`, 1)
	payload := `{"version": ` + itoa(version) + `, "gateway_id": "gw_1", "approval_timeout_s": 60, "policy": ` + pol + `,
		"agents": {"sre-bot": {"credentials": [{"type": "k8s_service_account", "name": "` + sreBot + `"}]}}}`
	env, _ := json.Marshal(bundle.Envelope{KeyID: "k1", Payload: base64.StdEncoding.EncodeToString([]byte(payload)),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(h.priv, []byte(payload)))})
	if err := h.store.Apply(env); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// admit sends one AdmissionReview and returns (allowed, code, message).
func admit(t *testing.T, srv *httptest.Server, user, op, kind, ns, name string) (bool, int, string) {
	req := map[string]any{"uid": "u1", "kind": map[string]any{"group": "", "version": "v1", "kind": kind}, "namespace": ns, "name": name,
		"operation": op, "userInfo": map[string]any{"username": user}}
	body, _ := json.Marshal(map[string]any{"apiVersion": "admission.k8s.io/v1", "kind": "AdmissionReview", "request": req})
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Response struct {
			UID     string
			Allowed bool
			Status  struct {
				Code    int
				Message string
			}
		}
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Response.UID != "u1" {
		t.Fatalf("response uid %q", out.Response.UID)
	}
	return out.Response.Allowed, out.Response.Status.Code, out.Response.Status.Message
}

func TestAdmission(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(Admission(h.engine))
	defer srv.Close()

	// 3 pod restarts per namespace in 10 minutes
	for i := range 3 {
		if ok, _, msg := admit(t, srv, sreBot, "DELETE", "Pod", "shop", "web-"+itoa(i)); !ok {
			t.Fatalf("restart %d: %s", i+1, msg)
		}
	}
	if ok, code, msg := admit(t, srv, sreBot, "DELETE", "Pod", "shop", "web-9"); ok || code != 429 || !strings.Contains(msg, "At most 3 pod restarts") {
		t.Errorf("4th restart: %v %d %q", ok, code, msg)
	}
	if ok, _, _ := admit(t, srv, sreBot, "DELETE", "Pod", "search", "a"); !ok {
		t.Error("another namespace has its own count")
	}

	// Credential rotation: refused at once naming the approval; the same request goes ahead once it's granted, once
	ok, _, msg := admit(t, srv, sreBot, "UPDATE", "Secret", "shop", "db-credentials")
	if ok || !strings.Contains(msg, "Waiting for approval") || !strings.Contains(msg, "Retry once it's approved") {
		t.Fatalf("first try: %v %q", ok, msg)
	}
	h.mu.Lock()
	id := h.asked[0]["approval_id"].(string)
	h.mu.Unlock()
	if !strings.Contains(msg, id) {
		t.Errorf("the message names the approval %s: %q", id, msg)
	}
	if ok, _, msg := admit(t, srv, sreBot, "UPDATE", "Secret", "shop", "db-credentials"); ok || !strings.Contains(msg, id) {
		t.Errorf("a retry before the answer waits on the same approval: %v %q", ok, msg)
	}
	if len(h.asked) != 1 {
		t.Errorf("asked %d times", len(h.asked))
	}
	raw, _ := json.Marshal(map[string]any{"approval_id": id, "granted": true, "by": "dana"})
	h.engine.Apply(sync.Command{Type: "approval", ID: "approval:" + id, Raw: raw})
	deadline := time.Now().Add(2 * time.Second)
	for ok, _, _ = admit(t, srv, sreBot, "UPDATE", "Secret", "shop", "db-credentials"); !ok && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		ok, _, _ = admit(t, srv, sreBot, "UPDATE", "Secret", "shop", "db-credentials")
	}
	if !ok {
		t.Fatal("approved retry refused")
	}
	if ok, _, msg := admit(t, srv, sreBot, "UPDATE", "Secret", "shop", "db-credentials"); ok || !strings.Contains(msg, "Waiting for approval") {
		t.Errorf("a grant is used once: %v %q", ok, msg)
	}

	// Someone the gateway doesn't serve
	if ok, _, msg := admit(t, srv, "system:serviceaccount:ops:other", "DELETE", "Pod", "shop", "x"); ok || !strings.Contains(msg, "isn't an agent") {
		t.Errorf("unknown service account: %v %q", ok, msg)
	}
}

// A fake RBAC API: one write RoleBinding for the agent and a teammate, one read binding, unlabeled
func TestContainment(t *testing.T) {
	h := setup(t)
	var mu gosync.Mutex
	write := map[string]any{"metadata": map[string]any{"name": "sre-write", "namespace": "shop", "resourceVersion": "1",
		"labels": map[string]any{"fences.dev/containment": "write"}},
		"roleRef": map[string]any{"kind": "Role", "name": "sre-write"},
		"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": "sre-bot", "namespace": "ops"},
			map[string]any{"kind": "User", "name": "ada"}}}
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer gw-token" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/rolebindings") && r.URL.Query().Get("labelSelector") == WriteLabel:
			json.NewEncoder(w).Encode(map[string]any{"items": []any{write}})
		case r.Method == "GET":
			json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
		case r.Method == "PUT" && r.URL.Path == "/apis/rbac.authorization.k8s.io/v1/namespaces/shop/rolebindings/sre-write":
			json.NewDecoder(r.Body).Decode(&write)
			json.NewEncoder(w).Encode(write)
		default:
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("gw-token\n"), 0o600)
	c := &Containment{Engine: h.engine, Cluster: &Cluster{Server: api.URL, TokenFile: tok, HTTP: api.Client()}}
	subjects := func() string {
		mu.Lock()
		defer mu.Unlock()
		b, _ := json.Marshal(write["subjects"])
		return string(b)
	}
	ctx := context.Background()

	if !c.Governs("sre-bot") || c.Governs("nobody") {
		t.Error("Governs")
	}
	if err := c.Reconcile(ctx, ""); err != nil || !strings.Contains(subjects(), "sre-bot") {
		t.Fatalf("not quarantined: nothing changes: %v %s", err, subjects())
	}
	if err := c.Reconcile(ctx, "sre-bot"); err == nil {
		t.Error("containment for an agent that isn't quarantined")
	}
	raw, _ := json.Marshal(map[string]any{"agent": "sre-bot", "bundle_version": 2})
	h.engine.Apply(sync.Command{Type: "quarantine", ID: "q1", Raw: raw})
	if err := c.Reconcile(ctx, "sre-bot"); err != nil {
		t.Fatalf("containment: %v", err)
	}
	if s := subjects(); strings.Contains(s, "sre-bot") || !strings.Contains(s, "ada") {
		t.Errorf("quarantined: the agent leaves the write binding, the teammate stays: %s", s)
	}
	if err := c.Reconcile(ctx, "sre-bot"); err != nil {
		t.Errorf("again: still contained: %v", err)
	}
	h.bundle(t, 2, `["sre-bot"]`) // the bundle takes over from the command: still quarantined
	c.Reconcile(ctx, "")
	if strings.Contains(subjects(), "sre-bot") {
		t.Error("released too early")
	}
	h.bundle(t, 3, `[]`) // released
	if err := c.Reconcile(ctx, ""); err != nil || !strings.Contains(subjects(), "sre-bot") {
		t.Errorf("released: the agent is back: %v %s", err, subjects())
	}
	mu.Lock()
	ann := write["metadata"].(map[string]any)["annotations"].(map[string]any)
	mu.Unlock()
	if _, left := ann[RemovedAnnotation]; left {
		t.Errorf("annotation left behind: %v", ann)
	}

	// Nothing labeled names the agent: containment says so instead of "ok"
	mu.Lock()
	write["subjects"] = []any{}
	mu.Unlock()
	h.engine.Apply(sync.Command{Type: "quarantine", ID: "q2", Raw: []byte(`{"agent": "sre-bot", "bundle_version": 4}`)})
	if err := c.Reconcile(ctx, "sre-bot"); err == nil || !strings.Contains(err.Error(), "no binding labeled") {
		t.Errorf("nothing to contain: %v", err)
	}
}

func TestAudit(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(Audit(h.engine, "audit-secret"))
	defer srv.Close()
	list := `{"kind": "EventList", "apiVersion": "audit.k8s.io/v1", "items": [
		{"stage": "ResponseComplete", "verb": "list", "user": {"username": "` + sreBot + `"}, "objectRef": {"resource": "pods", "namespace": "shop"},
		 "responseStatus": {"code": 200}, "stageTimestamp": "2026-10-31T12:00:00.000000Z"},
		{"stage": "ResponseStarted", "verb": "watch", "user": {"username": "` + sreBot + `"}, "objectRef": {"resource": "pods", "namespace": "shop"}},
		{"stage": "ResponseComplete", "verb": "delete", "user": {"username": "` + sreBot + `"}, "objectRef": {"resource": "pods", "namespace": "shop", "name": "x"}},
		{"stage": "ResponseComplete", "verb": "get", "user": {"username": "system:serviceaccount:kube-system:other"}, "objectRef": {"resource": "pods"}}]}`
	post := func(token string) int {
		req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(list))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if post("wrong") != 401 || h.queue.Depth() != 0 {
		t.Fatal("unauthenticated audit events must be refused")
	}
	if post("audit-secret") != 200 {
		t.Fatal("audit post")
	}
	events, _ := h.queue.Batch(10, 1<<20)
	if len(events) != 1 {
		t.Fatalf("only the agent's completed read: %d events", len(events))
	}
	var ev map[string]any
	json.Unmarshal(events[0], &ev)
	if ev["target"] != "list pods shop/" || ev["agent"] != "sre-bot" || ev["reason"] != AuditReason || ev["upstream_status"] != float64(200) || ev["at"] != float64(1793448000) {
		t.Errorf("event: %v", ev)
	}
}
