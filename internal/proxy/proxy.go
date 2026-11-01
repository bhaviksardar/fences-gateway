// Package proxy is the HTTP API gateway: a reverse proxy per API, at /http/{api}/{path...}, that agents call with a
// Fences-issued token. Each request is decided on its method, path, query and JSON body before it's forwarded.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/bhaviksardar/fences-gateway/internal/engine"
	"github.com/bhaviksardar/fences-gateway/pkg/policy"
)

// API is a real HTTP API, from the gateway's config. Its credentials (Headers) stay here, never the agent's.
type API struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// CallTimeout is how long a forwarded request may take, response included.
var CallTimeout = 2 * time.Minute

// Proxy serves every configured API.
type Proxy struct {
	Engine *engine.Engine
	APIs   map[string]API
}

// Handler serves /http/{api}/{path...}.
func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/http/{api}/{path...}", p.serve)
	return mux
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("api")
	api, ok := p.APIs[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	req := policy.Request{"on": "http", "at": now(), "server": name, "method": r.Method, "path": "/" + r.PathValue("path")}
	agent, ok := p.Engine.Agent(r.Header.Get("Authorization"), name) // a quarantined agent gets through: denied and recorded
	if !ok {
		p.Engine.Unauthenticated(req)
		w.Header().Set("WWW-Authenticate", `Bearer realm="fences"`)
		fail(w, http.StatusUnauthorized, "unknown credential, or not assigned "+name)
		return
	}
	req["agent"] = agent

	values, err := url.ParseQuery(r.URL.RawQuery)
	query := map[string]any{}
	for k, v := range values {
		if len(v) > 1 { // which copy the API reads is its choice: refuse rather than guess
			err = errRepeat
		}
		query[k] = v[0]
	}
	if err != nil {
		fail(w, http.StatusBadRequest, "query: "+err.Error())
		return
	}
	req["query"] = query
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		fail(w, http.StatusRequestEntityTooLarge, "request body: "+err.Error())
		return
	}
	// A body that is a JSON object is decided on whatever its Content-Type says: the API may parse it anyway
	if t := bytes.TrimSpace(raw); len(t) > 0 && t[0] == '{' {
		body, err := engine.ParseObject(raw)
		if err != nil {
			fail(w, http.StatusBadRequest, "request body: "+err.Error())
			return
		}
		req["body"] = body
	}

	call := p.Engine.Decide(r.Context(), req)
	if !call.Allowed() {
		call.Done(name, nil, 0)
		code := http.StatusForbidden
		if call.Decision.Decision == policy.RateLimited {
			code = http.StatusTooManyRequests
		}
		fail(w, code, call.Message())
		return
	}

	target, _ := url.Parse(api.URL) // checked when the config was loaded
	status := 0
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Path, pr.Out.URL.RawPath = req["path"].(string), "" // the path that was decided, re-encoded
			pr.SetURL(target)
			pr.Out.Header.Del("Authorization") // the agent's Fences token isn't the API's business
			for k, v := range api.Headers {
				pr.Out.Header.Set(k, v)
			}
			pr.Out.Body, pr.Out.ContentLength = io.NopCloser(bytes.NewReader(raw)), int64(len(raw))
		},
		ModifyResponse: func(resp *http.Response) error { status = resp.StatusCode; return nil },
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			status = http.StatusBadGateway
			fail(w, status, "upstream "+name+": "+err.Error())
		},
	}
	ctx, cancel := context.WithTimeout(r.Context(), CallTimeout)
	defer cancel()
	start := time.Now()
	rp.ServeHTTP(w, r.WithContext(ctx))
	call.Done(name, status, time.Since(start))
}

var errRepeat = errors.New("a parameter is repeated")

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func now() float64 { return float64(time.Now().UnixMilli()) / 1000 }
