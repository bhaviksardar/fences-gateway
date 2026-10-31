// Command fences-gateway governs agents at the point where they reach tools: today, MCP servers. See docs/ and
// spec/PROTOCOL-GATEWAY.md.
//
//	fences-gateway -config /etc/fences-gateway/config.json
//
// On first start it enrolls with FENCES_ENROLL_TOKEN (a one-time token from the dashboard).
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/bhaviksardar/fences-gateway/internal/approvals"
	"github.com/bhaviksardar/fences-gateway/internal/engine"
	"github.com/bhaviksardar/fences-gateway/internal/mcp"
	"github.com/bhaviksardar/fences-gateway/pkg/bundle"
	"github.com/bhaviksardar/fences-gateway/pkg/queue"
	"github.com/bhaviksardar/fences-gateway/pkg/sync"
)

var version = "dev" // set at release: -ldflags "-X main.version=0.1.0"

// Config is the gateway's config file. Header values may name environment variables (${CRM_TOKEN}), so secrets
// needn't be in the file.
type Config struct {
	Platform             string                  `json:"platform"`
	Name                 string                  `json:"name"`
	DataDir              string                  `json:"data_dir"`
	Listen               string                  `json:"listen"`
	Upstreams            map[string]mcp.Upstream `json:"upstreams"`
	ReportArgumentValues bool                    `json:"report_argument_values"`
	PinnedSigningKeys    map[string]string       `json:"pinned_signing_keys"` // key_id → base64; overrides enrollment's
	MaxQueuedEvents      int                     `json:"max_queued_events"`
}

func main() {
	path := flag.String("config", "/etc/fences-gateway/config.json", "config file")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *path, nil); err != nil {
		slog.Error("fences-gateway: " + err.Error())
		os.Exit(1)
	}
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // a misspelt setting is an error, not a silent default
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if err := sync.CheckURL(c.Platform); err != nil {
		return nil, fmt.Errorf("%s: platform: %w", path, err)
	}
	if c.Name == "" || c.DataDir == "" || len(c.Upstreams) == 0 {
		return nil, fmt.Errorf("%s: name, data_dir and at least one upstream are required", path)
	}
	for name, u := range c.Upstreams {
		if u.URL == "" || name == "" || filepath.Base(name) != name {
			return nil, fmt.Errorf("%s: upstream %q needs a url and a name without slashes", path, name)
		}
		for k, v := range u.Headers {
			u.Headers[k] = envRef.ReplaceAllStringFunc(v, func(ref string) string { return os.Getenv(ref[2 : len(ref)-1]) })
		}
	}
	return &c, nil
}

// envRef is ${NAME} in a header value. Only that form: a secret may contain a bare $.
var envRef = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// run serves until ctx ends. ready, if given, receives the address once the gateway is listening.
func run(ctx context.Context, path string, ready chan<- string) error {
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	client := &sync.Client{BaseURL: cfg.Platform, Kind: "gateway", Version: version}
	idPath := filepath.Join(cfg.DataDir, "identity.json")
	id, err := sync.LoadIdentity(idPath)
	if errors.Is(err, os.ErrNotExist) {
		token := os.Getenv("FENCES_ENROLL_TOKEN")
		if token == "" {
			return errors.New("not enrolled yet: set FENCES_ENROLL_TOKEN to a token from the dashboard")
		}
		if id, err = sync.Enroll(ctx, client, token, cfg.Name, idPath); err != nil {
			return fmt.Errorf("enrolling: %w", err)
		}
		slog.Info("fences-gateway: enrolled", "gateway_id", id.GatewayID)
	} else if err != nil {
		return err
	}
	client.ID = id

	keys := map[string]ed25519.PublicKey{}
	for k, v := range id.SigningKeys {
		keys[k] = v
	}
	if len(cfg.PinnedSigningKeys) > 0 { // a platform with swapped keys can't push a bundle
		keys = map[string]ed25519.PublicKey{}
		for k, v := range cfg.PinnedSigningKeys {
			key, err := base64.StdEncoding.DecodeString(v)
			if err != nil || len(key) != ed25519.PublicKeySize {
				return fmt.Errorf("pinned_signing_keys.%s isn't a base64 Ed25519 public key", k)
			}
			keys[k] = key
		}
	}
	store := &bundle.Store{Path: filepath.Join(cfg.DataDir, "bundle.json"), Keys: keys, GatewayID: id.GatewayID}
	if err := store.Load(); err != nil {
		slog.Error("fences-gateway: no usable bundle on disk; denying every request until one arrives", "err", err)
	}
	q, err := queue.Open(filepath.Join(cfg.DataDir, "events.db"), cfg.MaxQueuedEvents)
	if err != nil {
		return err
	}
	defer q.Close()

	eng := &engine.Engine{Store: store, Queue: q, Approvals: &approvals.Manager{Client: client},
		ReportValues: cfg.ReportArgumentValues, Version: version}
	syncer := &sync.Syncer{Client: client, Store: store, Health: eng.Health, Apply: eng.Apply, Urgent: eng.Approvals.Pending}
	gw := &mcp.Gateway{Engine: eng, Upstreams: cfg.Upstreams}
	defer gw.Close()

	mux := http.NewServeMux()
	mux.Handle("/mcp/", http.MaxBytesHandler(gw.Handler(), 4<<20))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if b := store.Current(); b != nil {
			fmt.Fprintf(w, "bundle %d\n", b.Version)
			return
		}
		http.Error(w, "no bundle yet", http.StatusServiceUnavailable)
	})
	srv := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	bg, cancel := context.WithCancel(ctx)
	defer cancel()
	go syncer.Run(bg)
	go sync.Ship(bg, client, q, nil)

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	if ready != nil {
		ready <- ln.Addr().String()
	}
	slog.Info("fences-gateway: serving", "listen", ln.Addr().String(), "upstreams", len(cfg.Upstreams), "version", version)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	return srv.Shutdown(shutdown)
}
