kagent's own `api.kagent.dev` Agent, RemoteMCPServer, ModelConfig and
EgressPolicy CRDs, from giantswarm/kagent-upstream `helm/kagent-crds/templates/`
(branch `giantswarm`, commit a894bfc5b1a496f452755488d94a09774c9ef5d8). The
envtest suites of `internal/agents` and `internal/organizations` serve them so
a dry-run is judged by the real schema and CEL rules; refresh them with a
kagent bump.

A refresh of the ModelConfig CRD runs
`TestModelSettingsMapMatchesTheModelConfigCRD` (`go test ./internal/agents/`,
no envtest needed). It fails when kagent adds, removes or renames a provider
block, or changes which block has `maxTokens`, `promptCaching` and `cacheTTL`;
its message names the entry of `providerBlockKeys` or `modelSettingsBlocks`
in `../../model_settings.go` to change. The Dev Portal keeps its own copy of
that map (giantswarm/backstage `plugins/agent-platform/src/lib/modelSettings.ts`),
which this test does not cover: change it there too.
