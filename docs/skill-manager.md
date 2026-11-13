# Pinning through skill-manager

With `--skill-manager-url` (chart `skillManager.url`) every pin goes through
[skill-manager](https://github.com/giantswarm/skill-manager)'s
`POST /v1/resolve`, skills and plugins in one call, as the caller (the bearer
agent-manager received): `create_agent`, `update_agent` (with
`refreshSkills`), `validate_agent` and `migrate` (skill-manager's service
token, `--skill-manager-token` / `SKILL_MANAGER_TOKEN`, as it has no
caller). skill-manager checks that the caller can read each repository and
that each path exists, and reports each git plugin's format for the Harness
guard. An entry with neither `ref` nor `commit` below a skill-manager
source's path pins that source's ref. `list_skills` and `GET /skills` proxy
its `GET /v1/skills` (deprecated: use skill-manager's tools). Its refusals
map to `403` (repository not readable), `400` (invalid item, unknown
repository, ref, path or tag), `401` (bearer refused) and `502`
(skill-manager, GitHub or a registry failing).

Without it, discovery and pinning run in process (deprecated).
