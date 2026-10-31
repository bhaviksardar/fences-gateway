// Package queue is the on-disk queue of events waiting to be sent to the platform (spec/PROTOCOL-GATEWAY.md, Decision
// events). It numbers each event with its seq, survives restarts, and never blocks a request because it's full.
//
// It is a public API, imported by fences-edge at a tagged version: no gateway-only imports here.
package queue

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"

	_ "modernc.org/sqlite" // pure Go, so the gateway cross-compiles without cgo
)

// DefaultMax is how many events the queue keeps while the platform is unreachable before it drops the oldest.
const DefaultMax = 100_000

// Queue is safe for concurrent use.
type Queue struct {
	db  *sql.DB
	max int

	mu      sync.Mutex
	depth   int
	dropped int64
	sent    int64 // the highest seq Batch has returned
}

// Open opens or creates the queue at path. max is how many events it keeps (DefaultMax if 0 or less).
func Open(path string, max int) (*Queue, error) {
	if max <= 0 {
		max = DefaultMax
	}
	// Created 0600 first: events can hold argument values. SQLite gives its -wal and -shm files the same mode.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("queue: %w", err)
	}
	f.Close()
	// ponytail: synchronous=NORMAL survives a crash of the process, but a power cut can lose the last few events and
	// let their seqs be reused (the platform would take them as repeats); FULL if that matters more than latency
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("queue: %w", err)
	}
	db.SetMaxOpenConns(1) // one writer; SQLite would serialize them anyway
	q := &Queue{db: db, max: max}
	// AUTOINCREMENT: a seq is never reused, even after the events up to it are deleted
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS events (seq INTEGER PRIMARY KEY AUTOINCREMENT, data BLOB NOT NULL)`)
	if err == nil {
		err = db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&q.depth)
	}
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("queue: %w", err)
	}
	return q, nil
}

// Close closes the queue's database.
func (q *Queue) Close() error { return q.db.Close() }

// Push adds an event (a JSON object without "seq") and returns the seq it was given. If the queue is over its max,
// the oldest events are dropped and counted in Dropped.
func (q *Queue) Push(event any) (int64, error) {
	data, err := json.Marshal(event)
	if err != nil || len(data) < 2 || data[0] != '{' {
		return 0, errors.New("queue: an event must be a JSON object")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	res, err := q.db.Exec(`INSERT INTO events (data) VALUES (?)`, data)
	if err != nil {
		return 0, fmt.Errorf("queue: %w", err)
	}
	seq, _ := res.LastInsertId()
	q.depth++
	if over := q.depth - q.max; over > 0 {
		res, err := q.db.Exec(`DELETE FROM events WHERE seq IN (SELECT seq FROM events ORDER BY seq LIMIT ?)`, over)
		if err == nil {
			n, _ := res.RowsAffected()
			q.depth -= int(n)
			q.dropped += n
		}
	}
	return seq, nil
}

// Batch returns up to n of the oldest events, each with its "seq" added, ready to send as {"events": [...]}.
func (q *Queue) Batch(n int) ([]json.RawMessage, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	rows, err := q.db.Query(`SELECT seq, data FROM events ORDER BY seq LIMIT ?`, n)
	if err != nil {
		return nil, fmt.Errorf("queue: %w", err)
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var seq int64
		var data []byte
		if err := rows.Scan(&seq, &data); err != nil {
			return nil, fmt.Errorf("queue: %w", err)
		}
		ev := []byte(`{"seq":` + strconv.FormatInt(seq, 10))
		if !bytes.Equal(data, []byte("{}")) {
			ev = append(ev, ',')
		}
		out = append(out, append(ev, data[1:]...))
		q.sent = max(q.sent, seq)
	}
	return out, rows.Err()
}

// Ack deletes every event up to and including seq: the platform's acked_seq. An acked_seq above the highest seq sent
// is taken as that seq, so a wrong answer can't delete events the platform never got.
func (q *Queue) Ack(seq int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	seq = min(seq, q.sent)
	res, err := q.db.Exec(`DELETE FROM events WHERE seq <= ?`, seq)
	if err != nil {
		return fmt.Errorf("queue: %w", err)
	}
	n, _ := res.RowsAffected()
	q.depth -= int(n)
	return nil
}

// Depth is how many events are waiting: health.queue_depth.
func (q *Queue) Depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.depth
}

// Dropped is how many events were dropped because the queue was full since it was opened: health.dropped_events.
func (q *Queue) Dropped() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}
