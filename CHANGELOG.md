# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

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
