package queue

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	batch, _ := q.Batch(10, 1<<20)
	var first, last map[string]any
	json.Unmarshal(batch[0], &first)
	json.Unmarshal(batch[3], &last)
	if len(batch) != 4 || first["seq"] != 1.0 || first["decision"] != "allow" || last["seq"] != 4.0 || len(last) != 1 {
		t.Fatalf("batch: %s", batch)
	}

	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("queue file mode %v, want 0600", info.Mode().Perm())
	}
	q.Push(struct{}{}) // seq 5, never sent: an ack past what was sent can't delete it
	q.Ack(99)
	if q.Depth() != 1 {
		t.Fatalf("ack past the batch deleted unsent events: depth %d", q.Depth())
	}

	// Acked events go, and their seqs are never given out again, even after a restart
	q.Batch(10, 1<<20)
	q.Ack(5)
	q.Close()
	q, _ = Open(path, 5)
	if seq, _ := q.Push(struct{}{}); seq != 6 || q.Depth() != 1 {
		t.Fatalf("after ack and restart: seq %d, depth %d; want 6, 1", seq, q.Depth())
	}

	// Over the max, the oldest are dropped and counted; pushing never fails because the queue is full
	for i := 0; i < 7; i++ {
		if _, err := q.Push(struct{}{}); err != nil {
			t.Fatal(err)
		}
	}
	batch, _ = q.Batch(10, 1<<20)
	json.Unmarshal(batch[0], &first)
	if q.Depth() != 5 || q.Dropped() != 3 || len(batch) != 5 || first["seq"] != 9.0 {
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

func TestBatchBytes(t *testing.T) {
	q, _ := Open(filepath.Join(t.TempDir(), "e.db"), 0)
	defer q.Close()
	big := map[string]string{"x": strings.Repeat("a", 1000)}
	for i := 0; i < 10; i++ {
		q.Push(big)
	}
	if b, _ := q.Batch(500, 3500); len(b) != 3 {
		t.Errorf("3,500 bytes holds 3 events of ~1 KB, got %d", len(b))
	}
	if b, _ := q.Batch(500, 10); len(b) != 1 {
		t.Errorf("the oldest event always goes, whatever its size: got %d", len(b))
	}
	q.Ack(99) // the batches above returned seqs 1-3, so only those can go
	if q.Depth() != 7 {
		t.Errorf("an ack can't reach past what Batch returned: depth %d, want 7", q.Depth())
	}
}
