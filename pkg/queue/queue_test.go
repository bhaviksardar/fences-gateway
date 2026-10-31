package queue

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bhaviksardar/fences-gateway/pkg/decisions"
)

func TestQueue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	q, err := Open(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Push("not an object"); err == nil {
		t.Error("a non-object event must be refused")
	}
	for i := 1; i <= 3; i++ {
		if seq, err := q.Push(decisions.Event{On: "mcp", Decision: "allow"}); err != nil || seq != int64(i) {
			t.Fatalf("seq %d, %v; want %d", seq, err, i)
		}
	}
	q.Push(struct{}{})
	batch, _ := q.Batch(10)
	var first, last map[string]any
	json.Unmarshal(batch[0], &first)
	json.Unmarshal(batch[3], &last)
	if len(batch) != 4 || first["seq"] != 1.0 || first["decision"] != "allow" || last["seq"] != 4.0 || len(last) != 1 {
		t.Fatalf("batch: %s", batch)
	}

	// Acked events go, and their seqs are never given out again, even after a restart
	q.Ack(4)
	q.Close()
	q, _ = Open(path, 5)
	if seq, _ := q.Push(struct{}{}); seq != 5 || q.Depth() != 1 {
		t.Fatalf("after ack and restart: seq %d, depth %d; want 5, 1", seq, q.Depth())
	}

	// Over the max, the oldest are dropped and counted; pushing never fails because the queue is full
	for i := 0; i < 7; i++ {
		if _, err := q.Push(struct{}{}); err != nil {
			t.Fatal(err)
		}
	}
	batch, _ = q.Batch(10)
	json.Unmarshal(batch[0], &first)
	if q.Depth() != 5 || q.Dropped() != 3 || len(batch) != 5 || first["seq"] != 8.0 {
		t.Fatalf("depth %d, dropped %d, oldest %v", q.Depth(), q.Dropped(), first["seq"])
	}

	// Concurrent pushes each get their own seq
	var wg sync.WaitGroup
	seen := sync.Map{}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seq, err := q.Push(struct{}{})
			if _, dup := seen.LoadOrStore(seq, true); dup || err != nil {
				t.Errorf("seq %d: dup %v, %v", seq, dup, err)
			}
		}()
	}
	wg.Wait()
	q.Close()
}
