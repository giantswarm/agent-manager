kagent's own `api.kagent.dev` Agent and RemoteMCPServer CRDs, from
giantswarm/kagent-upstream `helm/kagent-crds/templates/` (branch `giantswarm`,
commit a894bfc5b1a496f452755488d94a09774c9ef5d8). The envtest suite serves
them so a dry-run is judged by the real schema and CEL rules; refresh them with
a kagent bump.
