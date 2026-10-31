package queue

import (
	"path/filepath"
	"testing"

	"github.com/bhaviksardar/fences-gateway/pkg/decisions"
)

func BenchmarkPush(b *testing.B) {
	q, _ := Open(filepath.Join(b.TempDir(), "e.db"), 0)
	defer q.Close()
	ev := decisions.Event{On: "mcp", Target: "crm/get_ticket", Decision: "allow", Arguments: []string{"id"}}
	for b.Loop() {
		q.Push(ev)
	}
}
