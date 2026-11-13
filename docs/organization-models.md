# Organization models

An organization's admins write the organization's models (kagent
ModelConfigs in its namespace) with `set_organization_model`. The model's API
key is a Secret named by reference ([organization credential
operations](organization-credentials.md)); this page is about the model's
settings.

## Settings

| Argument | Where it is written | On a new model | On an update |
|---|---|---|---|
| `displayName` | annotation `ui.giantswarm.io/display-name`, the key the platform's models use | none unless given | omitted: kept; `""`: removed |
| `maxTokens` | `spec.<provider>.maxTokens`: `anthropic`, `anthropicVertexAI`, `azureOpenAI`, `mistral` or `openAI` | none unless given (the provider's default) | omitted: kept; `0`: removed |
| `promptCaching` | `spec.<provider>.promptCaching`: `anthropic` or `bedrock` | `true` for an Anthropic model, unless `false` is given | omitted: kept |
| `cacheTTL` | `spec.<provider>.cacheTTL`: `5m` or `1h` | `5m` with prompt caching on an Anthropic model | omitted: kept |

- A new Anthropic model caches its prompts, as the platform's Anthropic
  models do: a turn with a long system prompt or a long history reads them
  from the cache at a fraction of the input price. `promptCaching: false`
  creates it without caching. `1h` costs more for each cache write than `5m`;
  choose it only for an agent whose model calls are more than five minutes
  apart.
- An update writes only the arguments it is given. A model that has caching
  off stays without caching, also one written before this setting existed:
  give `promptCaching: true` to turn it on.
- A setting that the provider's block does not have (prompt caching on an
  OpenAI model, a token limit on an Ollama model) is refused with
  `invalid_request`: the API server would drop it and the model would run
  without it. A negative `maxTokens` is refused too. The API server's dry run
  judges the values themselves (the `cacheTTL` values, a minimum), in apply
  and commit mode.
- Apply mode writes the settings to the live object; commit mode writes the
  same fields into the model's file in git. An update in commit mode starts
  from the file in git: a setting changed on the live object by hand and not
  in git is not in the pull request.

`list_model_configs` and `GET /api/v1/modelconfigs` report each model's
`displayName`, `maxTokens` and `promptCaching` (with `cacheTTL` while caching
is on; no `promptCaching` for a provider without the setting), so an
organization's model compares with the platform's model of the same id.

## What an organization model cannot have

The platform's models (`kagent.modelConfigs` of the agent-platform chart, in
the `kagent` namespace) can also set these, and an organization model
written by `set_organization_model` cannot:

- `baseUrl`: an organization model always goes through the platform's LLM
  listener, which meters its calls. The chart's admission fills it in and
  refuses any other.
- `apiFormat` and `credentialType` (OpenAI): the responses API and a ChatGPT
  login.
- Other provider settings (temperature, top-p, top-k and the like).
- A provider that the LLM listener does not serve: admission refuses it.
