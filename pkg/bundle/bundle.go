// Package bundle checks signed policy bundles and keeps the last good one on disk (spec/PROTOCOL-GATEWAY.md, Bundles).
//
// It is a public API, imported by fences-edge at a tagged version: no gateway-only imports here.
package bundle

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/bhaviksardar/fences-gateway/pkg/policy"
)

// Envelope is a bundle as the platform sends it: the payload's exact JSON bytes and an Ed25519 signature over them.
type Envelope struct {
	KeyID     string `json:"key_id"`
	Payload   string `json:"payload"`   // base64
	Signature string `json:"signature"` // base64
}

// Bundle is a verified payload.
type Bundle struct {
	Version          int64            `json:"version"`
	GatewayID        string           `json:"gateway_id"`
	IssuedAt         float64          `json:"issued_at"`
	Agents           map[string]Agent `json:"agents"`
	ApprovalTimeoutS float64          `json:"approval_timeout_s"`
	Policy           *policy.Policy   `json:"-"`
}

// Agent is how the gateway recognizes one agent, and which upstreams it may reach.
type Agent struct {
	Credentials []Credential `json:"credentials"`
	Upstreams   []string     `json:"upstreams,omitempty"`
}

// Credential is one way an agent proves who it is. Type is "token", "header" or "k8s_service_account"; secrets appear
// only as SHA-256 hashes.
type Credential struct {
	Type   string `json:"type"`
	SHA256 string `json:"sha256,omitempty"`
	Header string `json:"header,omitempty"`
	Name   string `json:"name,omitempty"`
}

// Verify checks raw (an Envelope's JSON) against the trusted signing keys and this gateway's id, then parses it. It
// verifies the exact payload bytes before parsing them, and rejects an invalid policy. It doesn't check the version:
// Store does.
func Verify(raw []byte, keys map[string]ed25519.PublicKey, gatewayID string) (*Bundle, error) {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("bundle: %w", err)
	}
	key, ok := keys[env.KeyID]
	if !ok || len(key) != ed25519.PublicKeySize { // ed25519.Verify panics on a key of the wrong size
		return nil, fmt.Errorf("bundle: untrusted key_id %q", env.KeyID)
	}
	payload, err1 := base64.StdEncoding.DecodeString(env.Payload)
	sig, err2 := base64.StdEncoding.DecodeString(env.Signature)
	if err1 != nil || err2 != nil || !ed25519.Verify(key, payload, sig) {
		return nil, errors.New("bundle: bad signature")
	}
	var b struct {
		Bundle
		Policy json.RawMessage `json:"policy"`
	}
	if err := json.Unmarshal(payload, &b); err != nil {
		return nil, fmt.Errorf("bundle: %w", err)
	}
	if b.GatewayID != gatewayID {
		return nil, fmt.Errorf("bundle: for gateway %q, not this one", b.GatewayID)
	}
	var err error
	if b.Bundle.Policy, err = policy.Parse(b.Policy); err != nil {
		return nil, fmt.Errorf("bundle: %w", err)
	}
	return &b.Bundle, nil
}

// Store holds the current bundle and keeps it on disk at Path as the last good bundle. Current is safe to call from
// any goroutine and never blocks.
type Store struct {
	Path      string
	Keys      map[string]ed25519.PublicKey
	GatewayID string

	mu      sync.Mutex // serializes Load and Apply
	current atomic.Pointer[Bundle]
}

// Current is the bundle to decide with, or nil if there is none yet, in which case every request is denied
// (spec/PROTOCOL-GATEWAY.md, Fail mode). A request should call it once and keep the result, so it finishes under the
// bundle it started with.
func (s *Store) Current() *Bundle { return s.current.Load() }

// Load reads the last good bundle from disk at startup, checked like any other. A missing file is no error. On an
// error the store keeps no bundle; report it as health.bundle_error.
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	b, err := Verify(raw, s.Keys, s.GatewayID)
	if err != nil {
		return err
	}
	s.current.Store(b)
	return nil
}

// Apply accepts a bundle from the platform if it verifies and is newer than the current one, so a replayed bundle
// can't roll the policy back. On a verification error the current bundle stays. A newer bundle takes effect even if
// writing it to disk fails (it may carry a quarantine), and that error is returned for health.bundle_error.
func (s *Store) Apply(raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := Verify(raw, s.Keys, s.GatewayID)
	if err != nil {
		return err
	}
	if cur := s.current.Load(); cur != nil && b.Version <= cur.Version {
		return fmt.Errorf("bundle: version %d is not newer than %d", b.Version, cur.Version)
	}
	s.current.Store(b)
	if err := writeAtomic(s.Path, raw); err != nil {
		return fmt.Errorf("bundle: applied, but not saved: %w", err)
	}
	return nil
}

// writeAtomic writes a temp file next to path (mode 0600: bundles hold credential hashes) and renames it over path,
// so a crash leaves either the old bundle or the new one.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".bundle-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // fails harmlessly after the rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync() // the rename itself survives a crash
}
