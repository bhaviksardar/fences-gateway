// Package shared keeps limit counts in a store that speaks the Redis protocol (Redis, Valkey), so the replicas of a
// gateway group count together (spec/PROTOCOL-GATEWAY.md, Replicas). A request is added to its counts before it's
// decided and taken back out if it isn't allowed, so replicas racing for the last slot can both lose, never both win.
package shared

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Limit is one count a request falls under.
type Limit struct {
	Key     string  // the rule's signature and the request's limit key; hashed into the store's key
	WindowS float64 // the rule's window
	Amount  float64 // what the request adds to the total (0 without sum)
}

// Count is a limit's count and total before the request.
type Count struct {
	N     int
	Total float64
}

// Store is the shared store.
type Store struct {
	addr, user, pass string
	db               int
	tls              bool
	prefix           string
	timeout          time.Duration
	pool             chan *conn
}

// Open parses redis://[user:password@]host:port[/db] (rediss:// for TLS). Nothing is dialed until it's used.
func Open(rawURL, prefix string) (*Store, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "redis" && u.Scheme != "rediss") || u.Host == "" {
		return nil, errors.New("shared_limits: want redis://[user:password@]host:port[/db] or rediss://")
	}
	s := &Store{addr: u.Host, tls: u.Scheme == "rediss", prefix: prefix, timeout: 250 * time.Millisecond, pool: make(chan *conn, 32)}
	if !strings.Contains(u.Host, ":") {
		s.addr += ":6379"
	}
	if u.User != nil {
		s.user = u.User.Username()
		s.pass, _ = u.User.Password()
	}
	if db := strings.Trim(u.Path, "/"); db != "" {
		if s.db, err = strconv.Atoi(db); err != nil {
			return nil, fmt.Errorf("shared_limits: database %q isn't a number", db)
		}
	}
	if s.prefix == "" {
		s.prefix = "fences:limits:"
	}
	return s, nil
}

// reserve adds a request to its counts and returns them as they were before it: for each limit, the count and the
// total. Entries that left the window go first, and their amounts come off the total. Each limit is two keys, a
// sorted set of "member|amount" by time and the running total, under one hash tag so they share a cluster slot.
const reserve = `
local now, member = tonumber(ARGV[1]), ARGV[2]
local out = {}
for i = 1, #KEYS / 2 do
  local z, t = KEYS[2*i-1], KEYS[2*i]
  local window, amount = tonumber(ARGV[1+2*i]), ARGV[2+2*i]
  local old = redis.call('ZRANGEBYSCORE', z, '-inf', now - window)
  if #old > 0 then
    local gone = 0
    for _, m in ipairs(old) do gone = gone + tonumber(string.match(m, '|([^|]*)$')) end
    redis.call('ZREMRANGEBYSCORE', z, '-inf', now - window)
    redis.call('INCRBYFLOAT', t, -gone)
  end
  local n = redis.call('ZCARD', z)
  local total = '0'
  if n == 0 then redis.call('SET', t, '0') else total = redis.call('GET', t) or '0' end
  out[#out+1] = n
  out[#out+1] = total
  redis.call('ZADD', z, now, member .. '|' .. amount)
  redis.call('INCRBYFLOAT', t, amount)
  local ttl = math.ceil(window * 1000) + 60000
  redis.call('PEXPIRE', z, ttl)
  redis.call('PEXPIRE', t, ttl)
end
return out`

// release takes a request back out of its counts.
const release = `
for i = 1, #KEYS / 2 do
  if redis.call('ZREM', KEYS[2*i-1], ARGV[1] .. '|' .. ARGV[1+i]) == 1 then
    redis.call('INCRBYFLOAT', KEYS[2*i], -tonumber(ARGV[1+i]))
  end
end
return 1`

// Reserve adds a request (at, Unix seconds) to limits, and returns its member, to Release it if it isn't allowed,
// and the counts before it.
func (s *Store) Reserve(ctx context.Context, at float64, limits []Limit) (string, []Count, error) {
	b := make([]byte, 12)
	rand.Read(b)
	member := hex.EncodeToString(b)
	keys, args := s.keys(limits), []string{num(at), member}
	for _, l := range limits {
		args = append(args, num(l.WindowS), num(l.Amount))
	}
	reply, err := s.eval(ctx, reserve, keys, args)
	if err != nil {
		return "", nil, err
	}
	arr, ok := reply.([]any)
	if !ok || len(arr) != 2*len(limits) {
		return "", nil, fmt.Errorf("shared_limits: unexpected reply %v", reply)
	}
	counts := make([]Count, len(limits))
	for i := range counts {
		n, _ := arr[2*i].(int64)
		total, err := strconv.ParseFloat(fmt.Sprint(arr[2*i+1]), 64)
		if err != nil {
			return "", nil, fmt.Errorf("shared_limits: unexpected total %v", arr[2*i+1])
		}
		counts[i] = Count{N: int(n), Total: total}
	}
	return member, counts, nil
}

// Release takes a reserved request back out of its counts.
func (s *Store) Release(ctx context.Context, member string, limits []Limit) error {
	args := []string{member}
	for _, l := range limits {
		args = append(args, num(l.Amount))
	}
	_, err := s.eval(ctx, release, s.keys(limits), args)
	return err
}

func (s *Store) keys(limits []Limit) []string {
	keys := make([]string, 0, 2*len(limits))
	for _, l := range limits {
		sum := sha256.Sum256([]byte(l.Key))
		k := s.prefix + "{" + hex.EncodeToString(sum[:16]) + "}"
		keys = append(keys, k, k+":total")
	}
	return keys
}

func num(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

func (s *Store) eval(ctx context.Context, script string, keys, args []string) (any, error) {
	sum := sha1.Sum([]byte(script))
	cmd := append([]string{"EVALSHA", hex.EncodeToString(sum[:]), strconv.Itoa(len(keys))}, append(keys, args...)...)
	reply, err := s.do(ctx, cmd...)
	if err != nil && strings.HasPrefix(err.Error(), "NOSCRIPT") {
		cmd[0], cmd[1] = "EVAL", script
		reply, err = s.do(ctx, cmd...)
	}
	return reply, err
}

// ── The Redis protocol (RESP2), just enough for the above ────────────────────

type conn struct {
	net.Conn
	r *bufio.Reader
}

type replyError string

func (e replyError) Error() string { return string(e) }

func (s *Store) do(ctx context.Context, args ...string) (any, error) {
	c, err := s.get(ctx)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(s.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	c.SetDeadline(deadline)
	reply, err := c.call(args...)
	var re replyError
	if err != nil && !errors.As(err, &re) { // the connection is in an unknown state
		c.Close()
		return nil, err
	}
	select {
	case s.pool <- c:
	default:
		c.Close()
	}
	return reply, err
}

func (s *Store) get(ctx context.Context) (*conn, error) {
	select {
	case c := <-s.pool:
		return c, nil
	default:
	}
	d := net.Dialer{Timeout: s.timeout}
	var nc net.Conn
	var err error
	if s.tls {
		host, _, _ := net.SplitHostPort(s.addr)
		nc, err = (&tls.Dialer{NetDialer: &d, Config: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", s.addr)
	} else {
		nc, err = d.DialContext(ctx, "tcp", s.addr)
	}
	if err != nil {
		return nil, err
	}
	c := &conn{Conn: nc, r: bufio.NewReader(nc)}
	c.SetDeadline(time.Now().Add(s.timeout))
	if s.pass != "" {
		auth := []string{"AUTH", s.pass}
		if s.user != "" {
			auth = []string{"AUTH", s.user, s.pass}
		}
		if _, err := c.call(auth...); err != nil {
			c.Close()
			return nil, fmt.Errorf("shared_limits: %w", err)
		}
	}
	if s.db != 0 {
		if _, err := c.call("SELECT", strconv.Itoa(s.db)); err != nil {
			c.Close()
			return nil, fmt.Errorf("shared_limits: %w", err)
		}
	}
	return c, nil
}

func (c *conn) call(args ...string) (any, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := c.Write([]byte(b.String())); err != nil {
		return nil, err
	}
	return c.read()
}

func (c *conn) read() (any, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 3 {
		return nil, errors.New("shared_limits: malformed reply")
	}
	kind, body := line[0], line[1:len(line)-2]
	switch kind {
	case '+':
		return body, nil
	case '-':
		return nil, replyError(body)
	case ':':
		return strconv.ParseInt(body, 10, 64)
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil || n < 0 {
			return nil, err // -1: nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(c.r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(body)
		if err != nil || n < 0 {
			return nil, err
		}
		arr := make([]any, n)
		for i := range arr {
			v, err := c.read()
			var re replyError
			if err != nil && !errors.As(err, &re) {
				return nil, err
			}
			arr[i] = v
		}
		return arr, nil
	}
	return nil, fmt.Errorf("shared_limits: unknown reply type %q", kind)
}
