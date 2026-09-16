##@ Development

BINARY := agent-manager

.PHONY: build-linux-amd64
build-linux-amd64: ## Build the linux/amd64 binary the Dockerfile expects; version and commit come from the Go build info (the tag at HEAD, else dev).
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o $(BINARY)-linux-amd64 .

.PHONY: docker-build
docker-build: build-linux-amd64 ## Build a local dev image (TAG=agent-manager:dev).
	docker build --build-arg TARGETOS=linux --build-arg TARGETARCH=amd64 -t $(or $(TAG),agent-manager:dev) .

.PHONY: test-race
test-race: ## Run tests with the race detector.
	go test -race ./...

##@ Helm

.PHONY: helm-lint
helm-lint: ## Lint the chart.
	helm lint helm/agent-manager

.PHONY: helm-template
helm-template: ## Render the chart with defaults.
	helm template agent-manager helm/agent-manager

HELM_UNITTEST_VERSION := 1.0.3

.PHONY: helm-test
helm-test: helm-lint helm-unittest ## Run every chart check (what the chart-test CI job runs).

.PHONY: helm-unittest
helm-unittest: helm-plugin-unittest ## Run the helm-unittest suites in helm/agent-manager/tests/.
	helm unittest helm/agent-manager

.PHONY: helm-plugin-unittest
helm-plugin-unittest:
	@helm plugin list | grep -q '^unittest' || helm plugin install https://github.com/helm-unittest/helm-unittest --version $(HELM_UNITTEST_VERSION)

.PHONY: helm-schema
helm-schema: ## Regenerate values.schema.json (needs the helm schema plugin and schemalint).
	helm schema --config helm/agent-manager/.schema.yaml
	python3 -c 'import json,sys; h=lambda o: {**{k:v for k,v in o.items() if k!="additionalProperties"},"unevaluatedProperties":False} if ("$$ref" in o and o.get("additionalProperties") is False) else o; p=sys.argv[1]; f=open(p,encoding="utf-8"); d=json.load(f,object_hook=h); f.close(); f=open(p,"w",encoding="utf-8"); json.dump(d,f); f.close()' helm/agent-manager/values.schema.json
	schemalint normalize helm/agent-manager/values.schema.json -o helm/agent-manager/values.schema.json --force

.PHONY: helm-docs
helm-docs: ## Regenerate the chart README.
	helm-docs --chart-search-root=helm --sort-values-order=file
