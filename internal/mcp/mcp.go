// Package mcp is the MCP gateway: a remote MCP server per upstream, at /mcp/{upstream}, that agents connect to with a
// Fences-issued token. tools/list hides what the agent may never call; tools/call is decided before it's forwarded.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	gosync "sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bhaviksardar/fences-gateway/internal/engine"
	"github.com/bhaviksardar/fences-gateway/pkg/policy"
)

// Upstream is a real MCP server, from the gateway's config. Its credentials (Headers) stay here, never in a bundle,
// and are never the agent's.
type Upstream struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Limits on upstreams (decisions.md, threat model): how long a forwarded tools/call may take, and how big one
// response from an upstream may be.
var (
	CallTimeout = 2 * time.Minute
	MaxResponse = int64(16 << 20)
)

// Gateway serves the MCP surface.
type Gateway struct {
	Engine    *engine.Engine
	Upstreams map[string]Upstream

	mu       gosync.Mutex
	sessions map[string]*sdk.ClientSession // upstream name → the gateway's own session to it
}

// Handler serves /mcp/{upstream} for every configured upstream.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	for name := range g.Upstreams {
		srv := sdk.NewServer(&sdk.Implementation{Name: "fences-gateway/" + name, Version: g.Engine.Version},
			&sdk.ServerOptions{Capabilities: &sdk.ServerCapabilities{Tools: &sdk.ToolCapabilities{}}})
		srv.AddReceivingMiddleware(g.middleware(name))
		h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil)
		mux.Handle("/mcp/"+name, g.authenticate(name, h))
	}
	return mux
}

// authenticate refuses a request whose token matches no agent, or an agent not assigned this upstream, before any
// MCP session exists. The agent is found again on every request: a session id is never proof of who's calling.
func (g *Gateway) authenticate(upstream string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok := g.agent(r.Header, upstream) // a quarantined agent gets through: each of its calls is denied and recorded
		if !ok {
			g.Engine.Unauthenticated(policy.Request{"on": "mcp", "at": now(), "server": upstream, "tool": ""})
			w.Header().Set("WWW-Authenticate", `Bearer realm="fences"`)
			http.Error(w, "unknown credential", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// agent is the agent whose Fences token is in h, if it may reach upstream.
func (g *Gateway) agent(h http.Header, upstream string) (string, bool) {
	return g.Engine.Agent(h.Get("Authorization"), upstream)
}

func now() float64 { return float64(time.Now().UnixMilli()) / 1000 }

// middleware answers tools/list and tools/call from the upstream, through policy. Other methods (initialize, ping)
// are answered by the SDK; resources and prompts aren't proxied.
func (g *Gateway) middleware(upstream string) sdk.Middleware {
	return func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method != "tools/list" && method != "tools/call" {
				return next(ctx, method, req)
			}
			var header http.Header
			if extra := req.GetExtra(); extra != nil {
				header = extra.Header
			}
			agent, ok := g.agent(header, upstream)
			if !ok {
				return nil, fmt.Errorf("unknown credential")
			}
			if method == "tools/list" {
				return g.listTools(ctx, agent, upstream)
			}
			return g.callTool(ctx, agent, upstream, req.GetParams().(*sdk.CallToolParamsRaw))
		}
	}
}

func (g *Gateway) listTools(ctx context.Context, agent, upstream string) (*sdk.ListToolsResult, error) {
	s, err := g.session(ctx, upstream)
	if err != nil {
		return nil, err
	}
	b := g.Engine.Store.Current()
	out := &sdk.ListToolsResult{Tools: []*sdk.Tool{}}
	for tool, err := range s.Tools(ctx, nil) {
		if err != nil {
			g.drop(upstream, s)
			return nil, fmt.Errorf("upstream %s: %w", upstream, err)
		}
		// Annotations such as readOnlyHint come from the upstream and never affect a decision
		if b != nil && !g.Engine.Quarantined(agent) &&
			b.Policy.Possible(policy.Request{"agent": agent, "on": "mcp", "server": upstream, "tool": tool.Name}) {
			out.Tools = append(out.Tools, tool)
		}
	}
	return out, nil
}

func (g *Gateway) callTool(ctx context.Context, agent, upstream string, p *sdk.CallToolParamsRaw) (*sdk.CallToolResult, error) {
	args := map[string]any{}
	if len(p.Arguments) > 0 && string(p.Arguments) != "null" {
		var err error
		if args, err = engine.ParseObject(p.Arguments); err != nil {
			return nil, fmt.Errorf("tool arguments: %w", err)
		}
	}
	call := g.Engine.Decide(ctx, policy.Request{"agent": agent, "on": "mcp", "at": now(), "server": upstream, "tool": p.Name, "args": args})
	if !call.Allowed() {
		call.Done(upstream, nil, 0)
		return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: call.Message()}}}, nil
	}
	start := time.Now()
	res, err := g.forward(ctx, upstream, p)
	status := "ok"
	if err != nil || res.IsError {
		status = "error"
	}
	call.Done(upstream, status, time.Since(start))
	return res, err
}

func (g *Gateway) forward(ctx context.Context, upstream string, p *sdk.CallToolParamsRaw) (*sdk.CallToolResult, error) {
	s, err := g.session(ctx, upstream)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	res, err := s.CallTool(ctx, &sdk.CallToolParams{Name: p.Name, Arguments: p.Arguments})
	if err != nil {
		if ctx.Err() == nil {
			g.drop(upstream, s) // the session may be broken: reconnect next time
		}
		return nil, fmt.Errorf("upstream %s: %w", upstream, err)
	}
	return res, nil
}

// session is the gateway's own session to upstream, connected on first use with the upstream's credentials.
func (g *Gateway) session(ctx context.Context, upstream string) (*sdk.ClientSession, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if s := g.sessions[upstream]; s != nil {
		return s, nil
	}
	u := g.Upstreams[upstream]
	client := sdk.NewClient(&sdk.Implementation{Name: "fences-gateway", Version: g.Engine.Version}, nil)
	// ponytail: holds g.mu while connecting, so one slow upstream delays first connections to the others
	// Not the agent's request context: the session outlives the request that opened it
	s, err := client.Connect(context.WithoutCancel(ctx), &sdk.StreamableClientTransport{
		Endpoint:   u.URL,
		HTTPClient: &http.Client{Transport: headers{u.Headers, http.DefaultTransport}},
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("upstream %s: %w", upstream, err)
	}
	if g.sessions == nil {
		g.sessions = map[string]*sdk.ClientSession{}
	}
	g.sessions[upstream] = s
	return s, nil
}

// Close ends the gateway's sessions to its upstreams.
func (g *Gateway) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for name, s := range g.sessions {
		s.Close()
		delete(g.sessions, name)
	}
}

func (g *Gateway) drop(upstream string, s *sdk.ClientSession) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sessions[upstream] == s {
		delete(g.sessions, upstream)
		s.Close()
	}
}

// headers adds an upstream's own credentials to every request to it.
type headers struct {
	h    map[string]string
	next http.RoundTripper
}

func (t headers) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range t.h {
		r.Header.Set(k, v)
	}
	resp, err := t.next.RoundTrip(r)
	if err == nil && r.Method == http.MethodPost { // a reply to one request; the GET stream is long-lived by design
		resp.Body = &capped{resp.Body, MaxResponse + 1}
	}
	return resp, err
}

// capped fails a response body once it's over MaxResponse.
type capped struct {
	io.ReadCloser
	left int64
}

func (c *capped) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errors.New("upstream response is over the size limit")
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.ReadCloser.Read(p)
	c.left -= int64(n)
	if c.left <= 0 {
		return n, errors.New("upstream response is over the size limit")
	}
	return n, err
}
