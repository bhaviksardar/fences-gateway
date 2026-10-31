// Package decisions is the decision event the gateway reports for every request, and what it may say about the
// request's arguments (spec/PROTOCOL-GATEWAY.md, Decision events and Approvals).
//
// It is a public API, imported by fences-edge at a tagged version: no gateway-only imports here.
package decisions

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/bhaviksardar/fences-gateway/pkg/policy"
)

// Unauthenticated is the decision for a request whose credential matched no agent. Its event has no agent.
const Unauthenticated = "unauthenticated"

// Event is one decision, as sent to the platform. Pointer fields are null when unset.
type Event struct {
	Seq            int64    `json:"seq"`
	At             float64  `json:"at"`
	Agent          *string  `json:"agent"`
	On             string   `json:"on"`
	Target         string   `json:"target"`
	Arguments      any      `json:"arguments"` // from Arguments
	Decision       string   `json:"decision"`
	Rule           *string  `json:"rule"`
	Reason         *string  `json:"reason"`
	BundleVersion  int64    `json:"bundle_version"`
	Degraded       bool     `json:"degraded"`
	ApprovalID     *string  `json:"approval_id"`
	LatencyMS      float64  `json:"latency_ms"`
	UpstreamStatus any      `json:"upstream_status"` // "ok" or "error" for MCP, the status code for HTTP, nil if not forwarded
	UpstreamMS     *float64 `json:"upstream_ms"`
}

const (
	maxValue    = 300  // characters of one reported argument value
	maxArgs     = 300  // characters of arguments before each value is cut to its share, as in the SDK
	maxApproval = 2000 // characters of an approval summary, however many arguments
)

// Target names what a request reaches: "server/tool" (mcp), "METHOD /path" (http), "verb kind namespace/name" (k8s).
func Target(req policy.Request) string {
	s := func(k string) string { v, _ := req[k].(string); return v }
	switch s("on") {
	case "mcp":
		return s("server") + "/" + s("tool")
	case "http":
		return s("method") + " " + s("path")
	case "k8s":
		return s("verb") + " " + s("kind") + " " + s("namespace") + "/" + s("name")
	}
	return ""
}

// Arguments is what an event says about a request's arguments (an MCP call's args, an HTTP request's body fields):
// their sorted names by default, or with values, each value clipped to 300 characters.
func Arguments(req policy.Request, values bool) any {
	args := arguments(req)
	if !values {
		return append([]string{}, slices.Sorted(maps.Keys(args))...) // [] rather than null for no arguments
	}
	out := make(map[string]string, len(args))
	for k, v := range args {
		out[k] = cut(text(v), maxValue)
	}
	return out
}

func arguments(req policy.Request) map[string]any {
	key := map[string]string{"mcp": "args", "http": "body"}[fmt.Sprint(req["on"])]
	args, _ := req[key].(map[string]any)
	if args == nil {
		return map[string]any{}
	}
	return args
}

// Summary is the request, values included, for the person approving it: `POST /refunds {"amount":240,...}`. Every
// argument is shown: a long value is cut, never the arguments after it. Control, invisible and quote-like characters
// are escaped, so they can't hide text or pass for JSON's own quotes. At most 2,000 characters.
func Summary(req policy.Request) string {
	args := arguments(req)
	s := escape(marshal(args))
	if len([]rune(s)) > maxArgs {
		each := max(maxArgs/max(len(args), 1), 24)
		short := make(map[string]string, len(args))
		for k, v := range args {
			short[cut(k, each)] = cut(text(v), each)
		}
		s = escape(marshal(short))
	}
	return cut(Target(req)+" "+s, maxApproval)
}

// RequestHash is the SHA-256 of the request without `at`, as JSON with sorted keys and no whitespace. An approval
// applies only to a request with the same hash.
func RequestHash(req policy.Request) string {
	r := maps.Clone(req)
	delete(r, "at")
	sum := sha256.Sum256([]byte(marshal(r)))
	return hex.EncodeToString(sum[:])
}

// marshal is JSON with sorted keys, no whitespace and no HTML escaping. Values decoded from JSON always marshal.
func marshal(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func text(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return marshal(v)
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// escape writes control, format and quote-like characters as JSON escapes (”), as the SDK does.
func escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case unicode.In(r, unicode.Cc, unicode.Cf, unicode.Pi, unicode.Pf) || strings.ContainsRune("＂ʺ˝〃", r):
			if r > 0xFFFF {
				hi, lo := utf16.EncodeRune(r)
				fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
			} else {
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
