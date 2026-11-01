package k8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	gosync "sync"
	"time"

	"github.com/bhaviksardar/fences-gateway/internal/engine"
)

// Write bindings are the RoleBindings and ClusterRoleBindings labeled fences.dev/containment=write. Quarantine
// moves the agent's service account out of their subjects, into the RemovedAnnotation, and keeps every other
// binding (read access) as it is. A release moves it back. The state lives on the bindings, so a restart loses nothing.
const (
	WriteLabel        = "fences.dev/containment=write"
	RemovedAnnotation = "fences.dev/removed-subjects"
)

// Cluster is the Kubernetes API, as the gateway's own service account.
type Cluster struct {
	Server    string // https://host:port
	TokenFile string // read on every call: projected tokens rotate
	HTTP      *http.Client
}

// InCluster is the API server a pod reaches, with its service account's token and CA, unless server, tokenFile or
// caFile say otherwise.
func InCluster(server, tokenFile, caFile string) (*Cluster, error) {
	const sa = "/var/run/secrets/kubernetes.io/serviceaccount/"
	if server == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" {
			return nil, errors.New("kubernetes: no api_server, and not running in a cluster")
		}
		server = "https://" + host + ":" + port
		if strings.Contains(host, ":") {
			server = "https://[" + host + "]:" + port
		}
	}
	if tokenFile == "" {
		tokenFile = sa + "token"
	}
	if caFile == "" {
		caFile = sa + "ca.crt"
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("kubernetes: no certificates in %s", caFile)
	}
	return &Cluster{Server: server, TokenFile: tokenFile, HTTP: &http.Client{Timeout: 10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}}, nil
}

func (c *Cluster) do(ctx context.Context, method, path string, body, out any) error {
	token, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Server+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 300 {
		var st struct{ Message string }
		json.Unmarshal(data, &st)
		if st.Message == "" {
			st.Message = resp.Status
		}
		return errors.New(st.Message)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Containment keeps the write bindings in line with who is quarantined.
type Containment struct {
	Engine  *engine.Engine
	Cluster *Cluster
	Log     *slog.Logger

	mu gosync.Mutex // one pass at a time
}

// Run reconciles every interval until ctx ends: quarantines from a bundle and releases are picked up here.
func (c *Containment) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := c.Reconcile(ctx, ""); err != nil {
			c.log().Warn("fences: containment", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Containment) log() *slog.Logger {
	if c.Log == nil {
		return slog.Default()
	}
	return c.Log
}

// Governs says whether agent has a Kubernetes service account in the current bundle: whether a quarantine of it
// has a containment action.
func (c *Containment) Governs(agent string) bool {
	b := c.Engine.Store.Current()
	if b == nil {
		return false
	}
	for _, cr := range b.Agents[agent].Credentials {
		if _, _, ok := serviceAccount(cr.Name); cr.Type == "k8s_service_account" && ok {
			return true
		}
	}
	return false
}

// Reconcile moves quarantined agents' service accounts out of the write bindings and released ones back in. With
// agent set, it also says whether that agent ended up with no write binding naming it: the quarantine's containment
// result. An agent with no Kubernetes credential is skipped.
func (c *Containment) Reconcile(ctx context.Context, agent string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.Engine.Store.Current()
	if b == nil {
		if agent == "" {
			return nil // nothing known yet: nothing to change
		}
		return errors.New("no bundle yet")
	}
	quarantined := map[string]bool{} // "namespace/name" of a service account → its agent is quarantined
	target := ""
	for name, a := range b.Agents {
		for _, cr := range a.Credentials {
			ns, sa, ok := serviceAccount(cr.Name)
			if cr.Type != "k8s_service_account" || !ok {
				continue
			}
			quarantined[ns+"/"+sa] = c.Engine.Quarantined(name)
			if name == agent {
				target = ns + "/" + sa
			}
		}
	}
	if agent != "" && target == "" {
		return nil
	}
	if agent != "" && !quarantined[target] {
		return fmt.Errorf("%s isn't quarantined", agent)
	}

	sel := "?labelSelector=" + url.QueryEscape(WriteLabel)
	var errs []error
	found := 0 // write bindings that name the target, as a subject or a removed one
	for _, kind := range []string{"rolebindings", "clusterrolebindings"} {
		var list struct{ Items []map[string]any }
		if err := c.Cluster.do(ctx, "GET", "/apis/rbac.authorization.k8s.io/v1/"+kind+sel, nil, &list); err != nil {
			return fmt.Errorf("listing %s: %w", kind, err)
		}
		for _, item := range list.Items {
			changed, names := move(item, quarantined, target)
			if names {
				found++
			}
			if changed {
				meta, _ := item["metadata"].(map[string]any)
				path := "/apis/rbac.authorization.k8s.io/v1/"
				if ns, _ := meta["namespace"].(string); ns != "" {
					path += "namespaces/" + url.PathEscape(ns) + "/"
				}
				name, _ := meta["name"].(string)
				if err := c.Cluster.do(ctx, "PUT", path+kind+"/"+url.PathEscape(name), item, nil); err != nil {
					errs = append(errs, fmt.Errorf("%s %s: %w", kind, name, err))
				}
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if target != "" && found == 0 { // nothing was taken away: say so, rather than "ok"
		return fmt.Errorf("no binding labeled %s names %s", WriteLabel, target)
	}
	return nil
}

// move updates one binding: subjects of quarantined service accounts go to the annotation, released ones come back.
// names says whether the binding names target, as a subject or a removed one.
func move(item map[string]any, quarantined map[string]bool, target string) (changed, names bool) {
	meta, _ := item["metadata"].(map[string]any)
	ann, _ := meta["annotations"].(map[string]any)
	var removed []any
	if s, ok := ann[RemovedAnnotation].(string); ok {
		json.Unmarshal([]byte(s), &removed)
	}
	subjects, _ := item["subjects"].([]any)
	key := func(s any) (string, bool) {
		m, _ := s.(map[string]any)
		if m["kind"] != "ServiceAccount" {
			return "", false
		}
		ns, _ := m["namespace"].(string)
		name, _ := m["name"].(string)
		return ns + "/" + name, true
	}
	for _, s := range removed {
		k, ok := key(s)
		names = names || ok && k == target
	}
	var keep, back []any
	for _, s := range subjects {
		k, ok := key(s)
		names = names || ok && k == target
		if ok && quarantined[k] {
			removed, changed = append(removed, s), true
		} else {
			keep = append(keep, s)
		}
	}
	for _, s := range removed {
		if k, ok := key(s); ok && !quarantined[k] {
			if _, known := quarantined[k]; known { // only agents in this bundle are released; others stay out
				keep, changed = append(keep, s), true
				continue
			}
		}
		back = append(back, s)
	}
	if !changed {
		return false, names
	}
	item["subjects"] = keep
	if ann == nil {
		ann = map[string]any{}
		meta["annotations"] = ann
	}
	if len(back) == 0 {
		delete(ann, RemovedAnnotation)
	} else {
		b, _ := json.Marshal(back)
		ann[RemovedAnnotation] = string(b)
	}
	return true, names
}

// serviceAccount splits system:serviceaccount:<namespace>:<name>.
func serviceAccount(username string) (ns, name string, ok bool) {
	parts := strings.Split(username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" || parts[2] == "" || parts[3] == "" {
		return "", "", false
	}
	return parts[2], parts[3], true
}
