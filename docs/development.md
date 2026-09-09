# Developing on agent-manager

```sh
go build ./...          # the binary
go test ./...           # unit tests (fake dynamic + typed kube clients, httptest registry and GitHub)
make lint               # golangci-lint with the pre-commit linters (gosec, goconst, govet)
make helm-test          # helm lint + the helm-unittest suites in helm/agent-manager/tests (the chart-test CI job)
make helm-schema        # regenerate helm/agent-manager/values.schema.json
make helm-docs          # regenerate helm/agent-manager/README.md
```

## Layout

- `cmd/` — cobra CLI (`serve`, `version`); every flag has an environment
  variable. The flags of the retired Flux/agent-chart composition still parse
  as deprecated no-ops so an older chart release keeps starting the binary.
- `internal/kube` — the Kubernetes clients behind the `Client` / `Provider`
  interfaces: `CallerProvider` builds one client set per caller token
  (`rest.AnonymousClientConfig` + the bearer the OAuth layer put on the
  context; no ServiceAccount fallback), `ServiceAccountProvider` is the
  one shared client of a server without OAuth.
- `internal/identity` — the authenticated caller on the request context
  (subject, email, groups, source) and the IdP token downstream OAuth presents
  to the apiserver.
- `internal/agents` — the domain: `compose.go` composes the
  `kagent.dev/v1alpha3` AgentTemplate and the toolset carrier (the per-agent
  copy of the platform's muster RemoteMCPServer with the `X-Muster-Toolset`
  header), maps skills onto immutable sources and reads a served template back
  into its declaration; `harness.go` checks a Harness admits the template the
  way kagent's controller pairs them; `toolset.go` is the toolset grammar;
  `service.go` is list/get/create/update/delete plus model configs;
  `status.go` folds `status.harnesses[]` and the carrier into one verdict.
  `testdata/` holds the golden manifests (`UPDATE_GOLDEN=1 go test ./internal/agents`
  rewrites them).
- `internal/skills` — SKILL.md discovery in GitHub repositories, the portal
  backend's `/agent-skills` semantics, resolving the branch to the commit a
  template pins, with a per-repository cache.
- `internal/api` — REST handlers and MCP tools over the service.
- `internal/server` — the HTTP listener; `oauth.go` is the mcp-oauth resource
  server (Dex or Google provider, forwarded-id_token validation through
  `TrustedAudiences`, the caller onto the context) that guards the REST API
  and the MCP endpoint while the probes and the OAuth metadata stay public.
  `oauth_test.go` runs a fake OIDC issuer on `https://localhost` with a
  self-signed certificate — mcp-oauth rejects IP-literal issuers even with
  private IPs allowed.
- `api/openapi.yaml` — the REST contract; served at `/api/v1/openapi.yaml`.
- `helm/agent-manager` — the chart.

## Local loop against a kagent main kind cluster

```sh
go build -o agent-manager .
./agent-manager serve --listen 127.0.0.1:18080 \
  --kubeconfig ~/.kube/config --kube-context kind-agentlab --kagent-namespace kagent \
  --skills-repositories https://github.com/giantswarm/agent-skills -v

curl -s localhost:18080/api/v1/info
curl -s localhost:18080/api/v1/modelconfigs
curl -s -X POST localhost:18080/api/v1/agents/validate -d '{"name":"probe","modelConfig":"default-model-config","toolset":["preset:read-only"],"displayName":"Probe"}'
curl -s -X POST localhost:18080/api/v1/agents -d '{"name":"probe","modelConfig":"default-model-config","toolset":["preset:read-only"],"displayName":"Probe"}'
curl -s localhost:18080/api/v1/agents/kagent/probe/status
curl -s -X DELETE localhost:18080/api/v1/agents/kagent/probe
```

`validate` returns the composed manifests; `kubectl apply --dry-run=server -f`
on them validates the composition against the cluster's CRDs without writing.

## In the lab (agentlab)

```sh
make docker-build TAG=agent-manager:dev-$(git rev-parse --short HEAD)
kind load docker-image agent-manager:dev-$(git rev-parse --short HEAD) --name agentlab
helm upgrade --install agent-manager helm/agent-manager -n agent-platform \
  --set image.registry=docker.io --set image.repository=library/agent-manager \
  --set image.tag=dev-$(git rev-parse --short HEAD) --set image.pullPolicy=Never \
  --set muster.mcpServer.enabled=true
```

The lab muster then lists the tools as `x_agent-manager_*`; the proof is a
`create_agent` → `get_agent_status` (ready on the kagent Harness) →
`update_agent` → `delete_agent` round trip through `call_tool`, the
AgentTemplate binding its `muster-<agent>` carrier.

With the `agent-platform` meta chart the lab runs agent-manager as the
caller (`oauth.enabled` + `oauth.downstream.enabled` from the chart's
`agent-manager:` block, `requiredAudiences: [kubernetes]`, the `dex-localhost`
sidecar agentlab patches in through the component's `postRenderers` so the pod
reaches the lab issuer): swap the image through agentlab instead of installing
a second release (`platform.devImages.agent-manager: agent-manager:dev-<sha>`
in `agentlab.yaml`, then `agentlab platform`) and run
`agentlab agents-test` — the admin's round trip succeeds with
`requestedBy=admin@lab.local`, a `viewers`-group user's create is refused by
the apiserver as `User "oidc:viewer@lab.local"`, and
`kubectl auth can-i --list --as=system:serviceaccount:agent-platform:agent-manager -n kagent`
shows nothing beyond discovery.

The POC lab (`~/.local/state/kagent-main-poc`) runs kagent main from the fork
with the connectivity chart of `agent-platform` branch `poc/kagent-main`; the
dev image is swapped in through `platform.devImages.agent-manager`.
