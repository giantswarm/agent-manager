The Organization CRD (security.giantswarm.io), from
giantswarm/organization-operator `config/crd/bases/` at the version in go.mod.
The envtest suite of `internal/organizations` serves it beside kagent's CRDs
in `internal/agents/testdata/crds`, so the organization writes are judged by
the real schema; refresh it with the organization-operator bump in go.mod.
