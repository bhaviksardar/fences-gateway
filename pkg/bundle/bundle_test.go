package bundle

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const okPolicy = `{"default":"deny","rules":[{"id":"r","on":"mcp","effect":"allow"}]}`

func sign(t *testing.T, priv ed25519.PrivateKey, keyID, payload string) []byte {
	t.Helper()
	raw, _ := json.Marshal(Envelope{
		KeyID:     keyID,
		Payload:   base64.StdEncoding.EncodeToString([]byte(payload)),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(payload))),
	})
	return raw
}

func payload(version int, gateway, pol string) string {
	return `{"version":` + string(rune('0'+version)) + `,"gateway_id":"` + gateway + `","issued_at":1793610000,
		"agents":{"bot":{"credentials":[{"type":"token","sha256":"9c1e"}],"upstreams":["crm"]}},
		"policy":` + pol + `,"approval_timeout_s":600,"added_later":true}`
}

func TestStore(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	_, other, _ := ed25519.GenerateKey(nil)
	path := filepath.Join(t.TempDir(), "bundle.json")
	s := &Store{Path: path, Keys: map[string]ed25519.PublicKey{"k1": pub, "short": pub[:5]}, GatewayID: "gw_1"}

	if err := s.Load(); err != nil || s.Current() != nil {
		t.Fatalf("no file: want no bundle and no error, got %v, %v", s.Current(), err)
	}

	// Each refused, with nothing applied: fail closed
	for name, raw := range map[string][]byte{
		"unknown key":     sign(t, priv, "k9", payload(2, "gw_1", okPolicy)),
		"short key":       sign(t, priv, "short", payload(2, "gw_1", okPolicy)),
		"wrong signer":    sign(t, other, "k1", payload(2, "gw_1", okPolicy)),
		"other gateway":   sign(t, priv, "k1", payload(2, "gw_2", okPolicy)),
		"invalid policy":  sign(t, priv, "k1", payload(2, "gw_1", `{"default":"maybe"}`)),
		"no policy":       sign(t, priv, "k1", `{"version":2,"gateway_id":"gw_1"}`),
		"not json":        []byte("nope"),
		"payload not b64": []byte(`{"key_id":"k1","payload":"%%","signature":""}`),
	} {
		if err := s.Apply(raw); err == nil || s.Current() != nil {
			t.Errorf("%s: want refused, got %v", name, err)
		}
	}
	tampered := sign(t, priv, "k1", payload(2, "gw_1", okPolicy))
	tampered = []byte(strings.Replace(string(tampered), `"payload":"ey`, `"payload":"fy`, 1))
	if s.Apply(tampered) == nil {
		t.Error("tampered payload: want refused")
	}

	if err := s.Apply(sign(t, priv, "k1", payload(2, "gw_1", okPolicy))); err != nil {
		t.Fatal(err)
	}
	if b := s.Current(); b.Version != 2 || b.Agents["bot"].Upstreams[0] != "crm" || b.ApprovalTimeoutS != 600 ||
		b.Policy.Decide(map[string]any{"agent": "bot", "on": "mcp"}, nil, false).Decision != "allow" {
		t.Fatalf("applied bundle: %+v", b)
	}
	for _, v := range []int{2, 1} {
		if s.Apply(sign(t, priv, "k1", payload(v, "gw_1", okPolicy))) == nil {
			t.Errorf("version %d after 2: want refused (no rollback)", v)
		}
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("bundle file mode %v, want 0600", info.Mode().Perm())
	}

	// A restart picks up the last good bundle; a key it no longer trusts or a corrupted file leaves it with none
	s2 := &Store{Path: path, Keys: s.Keys, GatewayID: "gw_1"}
	if err := s2.Load(); err != nil || s2.Current().Version != 2 {
		t.Fatalf("reload: %v", err)
	}
	s3 := &Store{Path: path, Keys: map[string]ed25519.PublicKey{"k2": pub}, GatewayID: "gw_1"}
	if s3.Load() == nil || s3.Current() != nil {
		t.Error("reload with the key no longer trusted: want no bundle and an error")
	}
	os.WriteFile(path, []byte("garbage"), 0o600)
	s4 := &Store{Path: path, Keys: s.Keys, GatewayID: "gw_1"}
	if s4.Load() == nil || s4.Current() != nil {
		t.Error("corrupted file: want no bundle and an error")
	}

	// A disk that can't be written still gets the newer bundle in memory
	s.Path = filepath.Join(t.TempDir(), "missing-dir", "bundle.json")
	if err := s.Apply(sign(t, priv, "k1", payload(3, "gw_1", okPolicy))); err == nil || s.Current().Version != 3 {
		t.Errorf("unsaved bundle: want applied with an error, got version %d, %v", s.Current().Version, err)
	}
}
