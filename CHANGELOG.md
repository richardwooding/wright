# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- `internal/skillsdir`: Agent Skills discovery — merges
  `~/.config/wright/skills`, `<ws>/.agents/skills`, `<ws>/.wright/skills`,
  `skills.extraDirs` and (opt-in) `<ws>/.claude/skills`, later directories
  winning; a malformed `SKILL.md` is reported as a problem, not a failure.
  `wright skills` and `/skills` list what was found.
- `internal/mcpclient`: MCP servers behind consent and a trust record.
  Stdio servers run inside the OS sandbox with the filtered environment and
  no network unless declared; HTTP servers go through the ssrfguard client
  with configured headers. The first connection shows the command or URL and
  every tool with its annotations, then records the binary hash, arguments,
  URL and tool list in `trust.json`; any change re-asks with a diff. Tools
  register as `mcp_<server>_<tool>` and are addressed by policy as
  `mcp:<server>:<tool>`, with `readOnlyHint` carried through as advisory
  only. `wright mcp list|add|remove` and `/mcp`.
- `internal/agents`: the built-in read-only `explore` sub-agent and custom
  agents from `.wright/agents/*.md` (frontmatter `name`, `description`,
  `tools`, `model`, `read-only`; the body is the instructions). Sub-agents
  run through the engine's own middleware chain and are approved at depth 1
  by a child policy engine, so a child can never widen what the parent may
  do. `/agents`.
- `internal/engine`: `Engine.Middleware` exposes the approval/audit/untrusted
  chain for sub-agents, and `Approve` evaluates depth > 0 against
  `policy.Engine.Child`.
- `internal/config`: `mcpServers` entries gain `headers`, `prefix`, `network`
  and `trusted`; `Layered.SaveProject` writes `.wright/settings.json`.
- `internal/headless`: `wright -p` runner with `text`, `json` and
  `stream-json` output, the documented exit codes (0/1/2/3/4/130) and
  interrupt handling through the engine.
- `internal/app`: composition root — trust-gated settings, sandbox spec,
  policy layers with `.wright/settings.local.json` persistence, model and
  session selection, audit/snapshot/redaction wiring, the toolset with its
  engine adapters (ask_user, redaction notices, todo push, ssrfguard
  `web_fetch` client with redirect re-validation), `Run` with an
  `Interactive` hook, slash-command hook (`/diff /audit /init /trust
  /redaction`), `InitProject`; offline smoke tests against a fake
  OpenAI-compatible provider.
- `internal/cli`: `sessions list|show|export|delete|purge`, `models`,
  `config show`, `audit [id] [--kind] [--json]`, `audit verify`, `init`;
  `ExitError{Code}` maps headless exit codes to the process.
- `internal/enginetest`: shared scripted model client for tests.

- Scaffold: kong CLI (`--version`, `doctor`, `config paths`, hidden
  `__sandbox`), `internal/app` import-DAG and no-unexpected-network tests.
- `internal/theme`: lipgloss v2 palette from the gloam tokens as light/dark pairs.
- `internal/config`: layered settings (embedded defaults < user < project <
  project.local < env) with append-dedupe rule lists and atomic 0600 saves.
- `internal/workspace`: git-toplevel roots, symlink-safe `Resolve`,
  `.gitignore`/`.wrightignore`/`.aiignore`/`.aiexclude`, secret and protected
  path predicates.
- `internal/policy/shellclass`: bash-AST command classifier with wrapper
  peeling, `sh -c` recursion, opaque detection, hard-deny patterns and a
  command table covering coreutils, build tools, git, files, system
  administration, interpreters, containers, Kubernetes, IaC, databases and
  cloud CLIs.
- `internal/policy`: rule grammar, modes, verdict lattice, session/project
  grants, child engines, grant suggestions, builtin rules.
- `internal/redact`: high-confidence secret patterns with visible markers and
  a line-buffered writer.
- `internal/audit`: SHA-256-chained JSONL session log with `Verify` and an
  end-of-run `Summary`.
- `internal/snapshot`: content-addressed pre-edit snapshots per run with
  restore and pruning.
- `internal/trust`: accepted project settings and MCP servers by hash.
- `internal/git`: status summary, diff and tracked lookup with a 2 s timeout.
- `internal/sandbox`: backend detection (container, bwrap, landlock, seatbelt,
  none), allowlisted environment, bubblewrap profile with hidden `$HOME`,
  Landlock re-exec helper, macOS sandbox-exec profile generator.
- Release plumbing: GoReleaser (archives, `dockers_v2` image on ghcr.io,
  Homebrew cask), `Containerfile`, CI/release/pages/gloam-sync workflows,
  Dependabot, gloam docs site placeholder.
