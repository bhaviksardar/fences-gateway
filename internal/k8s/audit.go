package k8s

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bhaviksardar/fences-gateway/internal/engine"
	"github.com/bhaviksardar/fences-gateway/pkg/decisions"
	"github.com/bhaviksardar/fences-gateway/pkg/policy"
)

// AuditReason is the reason on an event recorded from the audit log: Fences saw it happen, it didn't decide it.
const AuditReason = "Seen in the cluster's audit log"

// Audit serves POST /k8s/audit, the API server's audit webhook backend (an audit.k8s.io/v1 EventList), with token as
// a Bearer token. The agents' reads (get, list, watch) go on their timelines: the admission webhook never sees them.
// Writes are left out, since the admission webhook already recorded each one when it decided it.
func Audit(e *engine.Engine, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unknown credential", http.StatusUnauthorized)
			return
		}
		data, err := io.ReadAll(r.Body)
		var list struct {
			Items []struct {
				Stage string `json:"stage"`
				Verb  string `json:"verb"`
				User  struct {
					Username string `json:"username"`
				} `json:"user"`
				ObjectRef *struct {
					Resource, Namespace, Name, APIGroup, Subresource string
				} `json:"objectRef"`
				ResponseStatus *struct {
					Code int `json:"code"`
				} `json:"responseStatus"`
				StageTimestamp time.Time `json:"stageTimestamp"`
			} `json:"items"`
		}
		if err == nil {
			err = json.Unmarshal(data, &list)
		}
		if err != nil {
			http.Error(w, "want an audit.k8s.io/v1 EventList", http.StatusBadRequest)
			return
		}
		b := e.Store.Current()
		for _, ev := range list.Items {
			if ev.Stage != "ResponseComplete" || ev.ObjectRef == nil || (ev.Verb != "get" && ev.Verb != "list" && ev.Verb != "watch") {
				continue
			}
			agent, ok := e.ServiceAccount(ev.User.Username)
			if !ok {
				continue
			}
			o := ev.ObjectRef
			// The audit log names the resource (pods), not the kind (Pod)
			req := policy.Request{"on": "k8s", "verb": ev.Verb, "group": o.APIGroup, "kind": o.Resource, "namespace": o.Namespace,
				"name": o.Name, "subresource": o.Subresource}
			reason := AuditReason
			out := decisions.Event{At: float64(ev.StageTimestamp.UnixMilli()) / 1000, Agent: &agent, On: "k8s",
				Target: decisions.Target(req), Decision: policy.Allow, Reason: &reason}
			if ev.StageTimestamp.IsZero() {
				out.At = float64(time.Now().UnixMilli()) / 1000
			}
			if b != nil {
				out.BundleVersion = b.Version
			}
			if ev.ResponseStatus != nil {
				out.UpstreamStatus = ev.ResponseStatus.Code
			}
			if _, err := e.Queue.Push(out); err != nil {
				http.Error(w, "couldn't queue the events", http.StatusServiceUnavailable) // the API server sends them again
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})
}
