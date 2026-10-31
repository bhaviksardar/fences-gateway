// Package approvals asks the platform for a person's approval and waits for the answer, which comes back as a command
// (spec/PROTOCOL-GATEWAY.md, Approvals).
package approvals

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	gosync "sync"
	"time"

	"github.com/bhaviksardar/fences-gateway/pkg/sync"
)

// ErrTimeout is returned by Ask when no answer came within the timeout.
var ErrTimeout = errors.New("approval timed out")

// Request is what the person approving sees, and what the approval applies to.
type Request struct {
	Agent       string `json:"agent"`
	On          string `json:"on"`
	Target      string `json:"target"`
	Rule        string `json:"rule"`
	Reason      string `json:"reason"`
	Summary     string `json:"summary"`
	RequestHash string `json:"request_hash"`
}

// Answer is a person's decision.
type Answer struct {
	Granted bool   `json:"granted"`
	By      string `json:"by"`
	Note    string `json:"note"`
}

// Manager tracks the approvals this gateway is waiting on.
type Manager struct {
	Client *sync.Client

	mu      gosync.Mutex
	pending map[string]chan Answer
}

// Ask sends an approval request and waits for the answer, the timeout, or ctx. id is the approval_id it chose. An
// error other than ErrTimeout or ctx's means the request couldn't be sent.
func (m *Manager) Ask(ctx context.Context, r Request, timeout time.Duration) (id string, a Answer, err error) {
	id = uuid()
	ch := make(chan Answer, 1)
	m.mu.Lock()
	if m.pending == nil {
		m.pending = map[string]chan Answer{}
	}
	m.pending[id] = ch
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.pending, id)
		m.mu.Unlock()
	}()

	body := struct {
		ApprovalID string `json:"approval_id"`
		Request
		TimeoutS float64 `json:"timeout_s"`
	}{id, r, timeout.Seconds()}
	if err := m.Client.Post(ctx, "/api/gateways/approvals", body, nil); err != nil {
		return id, Answer{}, fmt.Errorf("approvals: couldn't ask: %w", err)
	}
	select {
	case a := <-ch:
		return id, a, nil
	case <-time.After(timeout):
		return id, Answer{}, ErrTimeout
	case <-ctx.Done():
		return id, Answer{}, ctx.Err()
	}
}

// Answer delivers an approval command. An answer to an approval this gateway isn't waiting on is ignored.
func (m *Manager) Answer(cmd sync.Command) {
	var c struct {
		ApprovalID string `json:"approval_id"`
		Answer
	}
	if json.Unmarshal(cmd.Raw, &c) != nil {
		return
	}
	m.mu.Lock()
	ch := m.pending[c.ApprovalID]
	m.mu.Unlock()
	if ch != nil {
		select {
		case ch <- c.Answer:
		default: // already answered
		}
	}
}

// Pending says whether any approval is waiting, so the gateway syncs every 2 s meanwhile.
func (m *Manager) Pending() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending) > 0
}

func uuid() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
