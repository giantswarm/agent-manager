The AgentPlatformOrganization CRD, from giantswarm/agent-platform-operator
`helm/agent-platform-operator/crds/` at v0.4.0, the version in go.mod. The
envtest suite of `internal/organizations` serves it beside kagent's CRDs in
`internal/agents/testdata/crds`, so the organization writes are judged by the
real schema; refresh it with the agent-platform-operator bump in go.mod.
