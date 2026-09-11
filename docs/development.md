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

- `cmd/` — cobra CLI (`serve`, `version`); every flag has an environment variable.
- `internal/kube` — the Kubernetes clients behind the `Client` / `Provider`
  interfaces: `CallerProvider` builds one client set per caller token
  (`rest.AnonymousClientConfig` + the bearer the OAuth layer put on the
  context; no ServiceAccount fallback), `ServiceAccountProvider` is the
  one shared client of a server without OAuth.
- `internal/identity` — the authenticated caller on the request context
  (subject, email, groups, source) and the IdP token downstream OAuth presents
  to the apiserver.
- `internal/oci` — a minimal OCI distribution client (anonymous bearer
  challenge, tag list, one file out of a chart archive, the digest a tag
  resolves to) with a fake registry for tests in `ocitest`.
- `internal/chart` — the agent chart: the resolver that tracks the latest
  in-range version (`1.x`, Masterminds semantics like Flux) and its
  `values.schema.json`, and the embedded copy of the chart 1.x schema as the
  offline fallback.
- `internal/skills` — SKILL.md discovery in GitHub repositories (the portal
  backend's `/agent-skills` semantics, each skill with the head commit it was
  read at) and the `Resolver` that pins a branch or tag to its head commit and
  an image tag to its digest.
- `internal/agents` — the domain: `compose.go` mirrors the portal's
  `composeManifests.ts` (chart 1.x values, HelmRelease, OCIRepository),
  `skills.go` validates and pins skill entries, `validate.go` runs the chart
  schema, `service.go` is list/get/create/update/delete plus model configs —
  the read model comes from the AgentTemplate, the agent's RemoteMCPServer and
  the owning HelmRelease — and `status.go` folds the platform Harness's entry
  on the template, the HelmRelease and the Warning events into one verdict.
  `testdata/` holds what Generic chart 1.x renders.
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
- `.ats/main.yaml` — the ATS smoke: the chart installs on a kind cluster with
  the Flux and kagent.dev/v1alpha3 CRDs applied (pinned to the kagent line's
  commit), so the startup discovery path is covered.

## Local loop against a kind cluster with kagent API v2

```sh
go build -o agent-manager .
./agent-manager serve --listen 127.0.0.1:18080 \
  --kubeconfig <kubeconfig> --kagent-namespace kagent --harness-name kagent \
  --skills-repositories https://github.com/giantswarm/agent-skills -v

curl -s localhost:18080/api/v1/info
curl -s localhost:18080/api/v1/modelconfigs
curl -s localhost:18080/api/v1/skills
curl -s -X POST localhost:18080/api/v1/agents/validate -d '{"name":"probe","modelConfig":"default-model-config","displayName":"Probe","toolset":["preset:read-only"],"skills":[{"git":{"url":"https://github.com/giantswarm/agent-skills","ref":"main"},"path":"agent-self-awareness"}]}'
curl -s -X POST localhost:18080/api/v1/agents -d '{"name":"probe","modelConfig":"default-model-config","displayName":"Probe","toolset":["preset:read-only"]}'
curl -s localhost:18080/api/v1/agents/kagent/probe/status
curl -s -X PATCH localhost:18080/api/v1/agents/kagent/probe -d '{"refreshSkills":true}'
curl -s -X DELETE localhost:18080/api/v1/agents/kagent/probe
```

## In the lab (agentlab)

The lab installs the platform through the `agent-platform` meta chart; swap
this component in with `platform.devImages.agent-manager: <image:tag>` after
`make docker-build TAG=<image:tag>` — the image must run with the chart the lab
installs for agent-manager, so on a chart change point the component at the
branch's published chart build instead — and prove it with the lab's
`agents-test` and `toolsets-test` (see the agentlab README).
