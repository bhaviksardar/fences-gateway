// Package sync talks to the Fences platform for a gateway or a device: enrollment, signed requests, the sync loop
// that pulls bundles and commands, acknowledgements, and shipping queued events (spec/PROTOCOL-GATEWAY.md).
//
// It is a public API, imported by fences-edge at a tagged version: no gateway-only imports here.
package sync

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

// Errors from Post. Anything else (network, 429, 5xx, a reply that isn't JSON) is worth retrying with backoff.
var (
	ErrRevoked   = errors.New("sync: the platform refused this client's key (401)")
	ErrForbidden = errors.New("sync: the batch named an agent this client doesn't serve (403)")
	errRepeat    = errors.New("sync: a repeat the platform already has (409)")
	// ErrRejected is any other 4xx but 429: the platform will never accept this request as it is.
	ErrRejected = errors.New("sync: the platform rejected the request as it is (4xx)")
)

const maxReply = 32 << 20 // bytes; a bundle is the largest reply

// Identity is what enrollment gives a client, kept on disk (mode 0600) because it holds the private key.
type Identity struct {
	GatewayID     string             `json:"gateway_id"`
	PrivateKey    ed25519.PrivateKey `json:"private_key"`
	SigningKeys   map[string][]byte  `json:"signing_keys"` // the platform's bundle keys, by key_id
	SyncIntervalS float64            `json:"sync_interval_s"`
}

// Client sends signed requests to the platform. Kind is "gateway" or "device".
type Client struct {
	BaseURL string
	Kind    string
	Version string
	ID      *Identity
	HTTP    *http.Client // nil: a 5 s timeout and no redirects
}

// CheckURL accepts https, or plain http only to this machine: the platform's replies carry policy and commands.
func CheckURL(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil {
		return fmt.Errorf("sync: %q must be an https:// URL with no username", base)
	}
	if u.Scheme == "https" {
		return nil
	}
	if ip := net.ParseIP(u.Hostname()); u.Scheme == "http" && (u.Hostname() == "localhost" || ip != nil && ip.IsLoopback()) {
		return nil
	}
	return fmt.Errorf("sync: %q must use https", base)
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{
		Timeout: 5 * time.Second,
		// A redirect would carry a signed request wherever it points
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Enroll creates the client's key pair, enrolls it with a one-time token, and saves the identity at path.
func Enroll(ctx context.Context, c *Client, token, name, path string) (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	var reply struct {
		GatewayID   string `json:"gateway_id"`
		SigningKeys []struct {
			KeyID     string `json:"key_id"`
			PublicKey []byte `json:"public_key"` // base64
		} `json:"signing_keys"`
		SyncIntervalS float64 `json:"sync_interval_s"`
	}
	body := map[string]any{"token": token, "kind": c.Kind, "name": name, "public_key": []byte(pub), "version": c.Version}
	if err := c.post(ctx, "/api/gateways/enroll", body, &reply, false); err != nil {
		return nil, err
	}
	if reply.GatewayID == "" {
		return nil, errors.New("sync: enrollment reply has no gateway_id")
	}
	id := &Identity{GatewayID: reply.GatewayID, PrivateKey: priv, SigningKeys: map[string][]byte{}, SyncIntervalS: reply.SyncIntervalS}
	for _, k := range reply.SigningKeys {
		id.SigningKeys[k.KeyID] = k.PublicKey
	}
	data, _ := json.Marshal(id)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, fmt.Errorf("sync: enrolled as %s but couldn't save the identity: %w", id.GatewayID, err)
	}
	c.ID = id
	return id, nil
}

// LoadIdentity reads an identity saved by Enroll.
func LoadIdentity(path string) (*Identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("sync: %w", err)
	}
	var id Identity
	if err := json.Unmarshal(data, &id); err != nil || id.GatewayID == "" || len(id.PrivateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("sync: %s is not a saved identity", path)
	}
	return &id, nil
}

// Post sends body to path, signed with the client's key, and decodes the reply into out (if not nil). A 409 (a
// repeat the platform already has) counts as success.
func (c *Client) Post(ctx context.Context, path string, body, out any) error {
	if err := c.post(ctx, path, body, out, true); err != errRepeat {
		return err
	}
	return nil
}

func (c *Client) post(ctx context.Context, path string, body, out any, signed bool) error {
	if err := CheckURL(c.BaseURL); err != nil {
		return err
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Fences-Client", c.Kind+"/"+c.Version)
	if signed {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		sum := sha256.Sum256(data)
		msg := "POST\n" + path + "\n" + ts + "\n" + hex.EncodeToString(sum[:])
		req.Header.Set("X-Fences-Gateway-Id", c.ID.GatewayID)
		req.Header.Set("X-Fences-Timestamp", ts)
		req.Header.Set("X-Fences-Signature", base64.StdEncoding.EncodeToString(ed25519.Sign(c.ID.PrivateKey, []byte(msg))))
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	defer resp.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, maxReply))
	switch {
	case err != nil:
		return fmt.Errorf("sync: %w", err)
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrRevoked
	case resp.StatusCode == http.StatusForbidden:
		return ErrForbidden
	case resp.StatusCode == http.StatusConflict:
		return errRepeat
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests:
		return fmt.Errorf("%w: %s answered %d", ErrRejected, path, resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return fmt.Errorf("sync: %s answered %d", path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(reply, out); err != nil {
		return fmt.Errorf("sync: %s answered with something that isn't JSON", path)
	}
	return nil
}
