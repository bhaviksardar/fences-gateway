# Kubernetes agents through Fences Gateway

An AI agent that works on a cluster, such as an SRE agent restarting pods or rotating credentials, does it as a Kubernetes service account. Fences Gateway governs that service account in three ways:

- **Admission webhook.** Every write the agent makes (create, update, delete) is decided before the API server stores it. That covers limits such as "3 pod restarts per namespace every 10 minutes", approvals, and quarantine.
- **Containment.** On quarantine, the gateway takes the agent's service account out of its write role bindings. The agent keeps read access. A release puts it back.
- **Audit log** (optional). The agent's reads (get, list, watch) never reach admission, so the gateway records them from the cluster's audit webhook. Every action then shows on the agent's page.

## In Fences

1. **Add an agent** of kind **Third-party agent (Fences Gateway)**, and give it rules with `"on": "k8s"` (see below).
2. In **Settings → Gateways**, assign the agent to the gateway and fill in its **Kubernetes service account**, as the API server names it: `system:serviceaccount:<namespace>:<name>`.

## In the cluster

```sh
helm install fences-gateway deploy/helm/fences-gateway -n fences --create-namespace \
  --set config.platform=https://fences.example.com \
  --set enrollToken.secretName=fences-enroll \
  --set kubernetes.enabled=true \
  --set 'kubernetes.agents={system:serviceaccount:ops:sre-bot}' \
  --set 'kubernetes.writeRoles={sre-write}'
```

The chart:
- generates the webhook's TLS certificate once, and keeps it on upgrades;
- registers a validating webhook that only the listed service accounts' requests go to, through `matchConditions`. Everyone else's requests never wait on the gateway. Kubernetes 1.30 or later is needed.
- gives the gateway the RBAC containment needs (below).

`failurePolicy` is `Fail` by default: if the gateway can't be reached, the agents' writes are refused.

**Label the agent's write bindings** `fences.dev/containment=write`. Those are the RoleBindings or ClusterRoleBindings that let it change things. Leave the read bindings unlabeled.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: sre-write, namespace: shop, labels: {fences.dev/containment: write}}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: sre-write}
subjects: [{kind: ServiceAccount, name: sre-bot, namespace: ops}]
```

On quarantine, the gateway moves the service account from `subjects` into the binding's `fences.dev/removed-subjects` annotation, and moves it back when the agent is released. Other subjects of the binding are left alone. The gateway may update bindings, but may only **bind** the roles in `kubernetes.writeRoles`, which is what putting a subject back takes. If no labeled binding names the agent, the quarantine's containment is reported as failed, not ok. The webhook denies a quarantined agent's writes either way.

If a GitOps tool manages these bindings, it may put the subject back while the agent is quarantined. Exclude the `subjects` field from its sync, or rely on the webhook, which keeps denying the writes.

## Rules

A request is a `k8s` request (see [the rules spec](../spec/rules/README.md#requests)): `verb`, `group`, `kind`, `namespace`, `name`, `subresource`. The roadmap's AI SRE policy:

```json
{"default": "deny", "rules": [
  {"id": "restart-pods", "on": "k8s", "effect": "allow",
   "where": [{"field": "verb", "op": "is", "value": "delete"}, {"field": "kind", "op": "is", "value": "Pod"}],
   "limit": {"window_s": 600, "max": 3, "per": ["namespace"]}, "reason": "At most 3 pod restarts per namespace every 10 minutes"},
  {"id": "rotate-creds", "on": "k8s", "effect": "approve",
   "where": [{"field": "kind", "op": "is", "value": "Secret"}, {"field": "verb", "op": "is", "value": "update"},
             {"field": "name", "op": "matches", "value": "*-credentials"}], "reason": "Credential rotation needs approval"}
]}
```

- **A patch is an `update`.** The API server sends a patch to admission as an update, so a rule on `patch` never matches there.
- **Approvals.** An admission webhook can't hold a request open, so the first try is refused at once: `Fences: require_approval: Waiting for approval 0c3e…: Credential rotation needs approval. Retry once it's approved.` The request goes to Slack and the dashboard. Once someone approves it, the same request (same verb, object and namespace) goes ahead if retried within 10 minutes. Each approval is good for one retry.
- With `default: deny`, anything you don't allow is refused. That includes creating objects the agent usually makes, so list them.

## Audit log (optional)

Set `kubernetes.auditToken.secretName` to a Secret holding a random token. Then configure the API server's [audit webhook backend](https://kubernetes.io/docs/tasks/debug/debug-cluster/audit/#webhook-backend) to post to `https://<gateway>.<namespace>.svc:8080/k8s/audit`, with that token as its bearer token. The gateway records the governed agents' completed reads with the reason "Seen in the cluster's audit log". They show on the agent's page, and they don't count against limits. A managed cluster that doesn't let you set the API server's flags can skip this: the admission webhook still decides every write.
