# Developing on agent-manager

```sh
go build ./...          # the binary
go test ./...           # unit tests (fake dynamic + typed kube clients, httptest registry and GitHub)
make lint               # golangci-lint with the pre-commit linters (gosec, goconst, govet)
make helm-schema        # regenerate helm/agent-manager/values.schema.json
make helm-docs          # regenerate helm/agent-manager/README.md
```

## Layout

- `cmd/` — cobra CLI (`serve`, `version`); every flag has an environment variable.
- `internal/kube` — the Kubernetes client behind the `Client` / `Provider`
  interfaces. `ServiceAccountProvider` is today's identity; a caller-identity
  provider (per-call client from the bearer token, muster's `callerwrite`
  pattern) plugs in here without touching the service.
- `internal/chart` — the agent chart: a minimal OCI distribution client
  (anonymous bearer challenge, tag list, one file out of the chart archive), the
  resolver that tracks the latest in-range version and its `values.schema.json`,
  and the embedded copy of the schema as the offline fallback.
- `internal/agents` — the domain: `compose.go` mirrors the portal's
  `composeManifests.ts` (values, HelmRelease, OCIRepository), `validate.go`
  runs the chart schema, `service.go` is list/get/create/update/delete plus
  model configs, `status.go` folds Agent, HelmRelease, Deployment, pods and
  events into one verdict.
- `internal/skills` — SKILL.md discovery in GitHub repositories, the portal
  backend's `/agent-skills` semantics, with a per-repository cache.
- `internal/api` — REST handlers and MCP tools over the service.
- `internal/server` — the HTTP listener.
- `api/openapi.yaml` — the REST contract; served at `/api/v1/openapi.yaml`.
- `helm/agent-manager` — the chart.

## Local loop against the agentlab kind cluster

```sh
go build -o agent-manager .
./agent-manager serve --listen 127.0.0.1:18080 \
  --kubeconfig ~/.kube/config --kube-context kind-agentlab --kagent-namespace kagent \
  --skills-repositories https://github.com/giantswarm/agent-skills -v

curl -s localhost:18080/api/v1/info
curl -s localhost:18080/api/v1/modelconfigs
curl -s -X POST localhost:18080/api/v1/agents/validate -d '{"name":"probe","modelConfig":"default-model-config","displayName":"Probe"}'
curl -s -X POST localhost:18080/api/v1/agents -d '{"name":"probe","modelConfig":"default-model-config","displayName":"Probe"}'
curl -s localhost:18080/api/v1/agents/kagent/probe/status
curl -s -X DELETE localhost:18080/api/v1/agents/kagent/probe
```

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
`create_agent` → `get_agent_status` (ready) → `delete_agent` round trip
through `call_tool` while another agent keeps the shared OCIRepository alive.
