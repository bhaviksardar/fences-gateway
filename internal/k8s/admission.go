// Package k8s governs AI agents' Kubernetes service accounts: a validating admission webhook decides their writes,
// containment removes their write role bindings on quarantine, and an audit webhook records their reads.
package k8s

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bhaviksardar/fences-gateway/internal/engine"
	"github.com/bhaviksardar/fences-gateway/pkg/policy"
)

// review is the part of an admission.k8s.io/v1 AdmissionReview the gateway reads.
type review struct {
	Request *struct {
		UID         string `json:"uid"`
		Kind        struct{ Group, Kind string }
		SubResource string `json:"subResource"`
		Name        string `json:"name"`
		Namespace   string `json:"namespace"`
		Operation   string `json:"operation"`
		UserInfo    struct {
			Username string `json:"username"`
		} `json:"userInfo"`
	} `json:"request"`
}

// Admission serves POST /k8s/admit, the validating webhook. Only the agents' service accounts should be sent to it
// (the webhook's matchConditions): anyone else is refused as an unknown credential.
func Admission(e *engine.Engine) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		var rv review
		if err == nil {
			err = json.Unmarshal(data, &rv)
		}
		if err != nil || rv.Request == nil || rv.Request.UID == "" {
			http.Error(w, "want an admission.k8s.io/v1 AdmissionReview", http.StatusBadRequest)
			return
		}
		q := rv.Request
		// CREATE, UPDATE, DELETE, CONNECT. A patch arrives as UPDATE: the API server doesn't tell admission which it was
		verb := strings.ToLower(q.Operation)
		req := policy.Request{"on": "k8s", "at": float64(time.Now().UnixMilli()) / 1000, "verb": verb, "group": q.Kind.Group,
			"kind": q.Kind.Kind, "namespace": q.Namespace, "name": q.Name, "subresource": q.SubResource}

		allowed, code, msg := false, http.StatusForbidden, ""
		if agent, ok := e.ServiceAccount(q.UserInfo.Username); !ok {
			e.Unauthenticated(req)
			msg = "Fences: " + q.UserInfo.Username + " isn't an agent this gateway serves"
		} else {
			// ponytail: a dry run is decided and counted like the real thing; skip reserving if agents dry-run a lot
			req["agent"] = agent
			call := e.Decide(r.Context(), req)
			call.Done("", nil, 0)
			allowed, msg = call.Allowed(), call.Message()
			if call.Decision.Decision == policy.RateLimited {
				code = http.StatusTooManyRequests
			}
		}
		resp := map[string]any{"uid": q.UID, "allowed": allowed}
		if !allowed {
			resp["status"] = map[string]any{"code": code, "message": msg}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"apiVersion": "admission.k8s.io/v1", "kind": "AdmissionReview", "response": resp})
	})
}
