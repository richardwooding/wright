# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`wright` is a coding-agent terminal harness (`module github.com/richardwooding/wright`,
Go 1.27, pure Go, no cgo): a Bubble Tea v2 TUI and a headless mode around an
`agentkit` agent, with a permission engine, an OS sandbox, secret redaction and a
tamper-evident audit log between the model and the machine. Generic agent/LLM
plumbing belongs in `agentkit`/`llmkit`; everything coding-specific lives here.

## Commands

```sh
go build ./... && go vet ./... && go fix -diff ./...
go test -race ./...                                   # offline; sandbox tests skip when bwrap/landlock are unavailable
go test -race -run TestEvaluateTable ./internal/policy   # one table
go test -fuzz=FuzzAnalyze -fuzztime=30s ./internal/policy/shellclass
go test -fuzz=FuzzParseRule -fuzztime=30s ./internal/policy
go test -fuzz=FuzzRedact -fuzztime=30s ./internal/redact
go test -run 'TestImportDAG|TestNoUnexpectedNetwork' ./internal/app -v

gofumpt -l . && golangci-lint run ./...               # the bar is 0 issues (default linters + gocyclo/gocognit/goconst)
go run ./cmd/wright doctor                            # sandbox backends, landlock ABI, tools, credential names

goreleaser check
goreleaser release --snapshot --clean --skip=publish,docker   # local release dry run (docker needs buildx; use podman below)
CGO_ENABLED=0 GOOS=linux go build -trimpath -o dist/wright-local ./cmd/wright && podman build -f Containerfile.local -t wright:dev .
```

`~/Projects/Personal/go.work` lists the sibling libraries but not this repo, so
run Go commands here with `GOWORK=off` (or add `./wright` to the workspace).
Never add `replace` directives.

## Architecture

```
cmd/wright/main.go        kong parse, signal.NotifyContext, exit codes; version/commit/date via ldflags
internal/
  cli/        kong CLI struct + Run methods; the only importer of app; hidden `__sandbox` landlock helper
  app/        composition root (config → model → tools → policy → engine → tui | headless); DAG/network tests
  engine/     (next) wraps agentkit: runs, fan-in Event channel, Approver, Inbox steering
  tui/        (next) Bubble Tea v2 root model; imports engine/theme/config/session/model/cost/git only
  headless/   (next) -p mode: text | json | stream-json
  tools/      (next) agentkit.Func tools + Describer (policy.Request + Preview)
  policy/     rule grammar, modes, verdict lattice, hard-deny set, grants, child engines
  policy/shellclass/  mvdan.cc/sh AST → per-command class; Unknown/HardDeny; leaf package (interface Workspace)
  sandbox/    Backend: container | bwrap | landlock | seatbelt | none; filtered Env(); __sandbox Helper
  workspace/  roots + extra dirs, symlink-safe Resolve, .gitignore/.wrightignore, secret/protected paths
  redact/     high-confidence secret patterns → "[redacted: <name>…last4]"; line-buffered Writer
  audit/      SHA-256-chained JSONL per session: Open/Write/Read/Verify/Summarize
  snapshot/   content-addressed pre-edit snapshots per run → /undo
  config/     Settings layers (embedded defaults.json < user < project < project.local < env), atomic saves
  trust/      accepted project settings / MCP servers by hash, ~/.config/wright/trust.json
  git/        exec git: Status (2 s timeout), Diff, IsTracked
  theme/      lipgloss v2 palette (gloam tokens as LightDark pairs) + styles
docs/         gloam Pages site (gloam.css/gloam.js vendored; sync-gloam.sh + gloam-sync.yml keep them current)
```

Import DAG, enforced by `TestImportDAG` in `internal/app` over `go list -deps`:
`tui` never imports `tools`, `policy`, `sandbox`; `tools` never imports `tui`
or `engine`; `policy` never imports `tui`, `engine`, `tools`; `shellclass`
imports nothing internal (it takes a small `Workspace` interface that
`policy.NewShellWorkspace` adapts). `engine` is the seam between agent and UI.
`TestNoUnexpectedNetwork` allowlists the internal packages that may import
`net/http` (only `model`, for the Ollama loopback probe).

### Things that are non-obvious and easy to break

- **Shell matching is AST-only.** `shellclass.Analyze` parses with
  `mvdan.cc/sh/v3/syntax` and classifies each simple command. Never add a
  regex-on-the-raw-string rule: `git status; rm -rf ~` must be seen as two
  commands, `$(…)` must be walked, quoting must be respected. Anything the
  analyser cannot see through sets `Unknown`, and `policy` refuses to match
  allow rules against Unknown scripts — that is the property that makes allow
  rules safe to write.
- **The hard-deny set is a floor, not a rule.** It is checked before rules and
  before the mode table, including in bypass mode, and it counts
  (`Engine.HardDenials`; the run is cancelled after three). Add new entries in
  `shellclass` (for commands) or `eval.hardDenyPaths` (for paths), never as a
  builtin deny rule, because rules can be shadowed by mode.
- **Ask rules have two ranks.** Explicit ask rules (user/project/flag) sit
  above allow rules so tightening always wins. The *builtin* ask list is the
  mode table's defaults written as rules and sits *below* allow rules —
  otherwise no allow rule for `edit_file`/`web_fetch`/`git push` could ever take
  effect. `TestEvaluateTable` pins both directions.
- **Builtin rules and `config/defaults.json` must match.** `policy/builtin.go`
  and the embedded JSON are the same list; `TestBuiltinMatchesConfigDefaults`
  fails if they drift.
- **Bypass is never inherited or settable.** `Engine.SetMode(ModeBypass)`
  returns `ErrBypassNotSettable`; `Child` clamps bypass to default; children
  cannot `Grant`. Only `--bypass-permissions` at construction enables it.
- **Symlink-safe paths.** `workspace.Resolve` evaluates symlinks on the deepest
  existing ancestor and appends the rest, so `ws/link/x` where `link → ~/.ssh`
  resolves outside. Every path in a `policy.Request` must have gone through it.
- **The sandbox hides `$HOME`.** bwrap mounts a tmpfs over the home directory
  *before* re-binding workspace roots and caches (order matters). The landlock
  helper lists system directories explicitly (`systemRO`) — never
  `RODirs("/")`, which would expose `/home`. Device nodes go through
  `RWFiles`, not `RWDirs` (Landlock rejects directory rights on files).
- **`sandbox.Env` is an allowlist with a hard strip.** Passthrough from trusted
  settings can add names, but nothing matching the strip regexes
  (`_KEY|_TOKEN|_SECRET|_PASSWORD`, `AWS_`, provider prefixes…) ever reaches a
  sandboxed command, even if passed through.
- **Redaction must be idempotent.** `Redact(Redact(x)) == Redact(x)` is
  fuzzed. Default patterns must not be able to match inside a marker: token
  character classes exclude `[`, and no vendor prefix appears in a marker
  name. The generic entropy pattern stays opt-in (`WithGeneric`).
- **Audit lines are immutable.** `audit.Log.Write` sets `Seq` and `Prev` (the
  SHA-256 of the previous *line*), redacts `Text`/`Args`, caps `Args` at 4 KiB
  and records `ArgsSHA256` of the full value. Never log environment variables
  or request bodies. `Verify` walks the chain; a whole-line truncation at the
  tail is not detectable by the chain alone (the `run_end` event is the
  witness), and the tests say so.
- **Config lists accumulate.** `config.Merge` appends+dedupes `allow/ask/deny`
  (and other slices) and overrides scalars only when non-zero; booleans that
  default to true are `*bool` so a later layer can turn them off.
- **Doctor prints credential *names* only.** Never print an environment value.

### Why these choices

- **`charm.land/...` import paths.** The Charm v2 modules declare
  `charm.land/bubbletea/v2` etc. in their `go.mod`; the `github.com/charmbracelet/.../v2`
  paths fail with a module-path mismatch. lipgloss v2 uses `LightDark(isDark)`
  pairs instead of `AdaptiveColor`, which is why `theme.New` takes `isDark`.
- **`dockers_v2` + `Containerfile`, not `kos:`.** The house style is ko on a
  static base, but the agent shells out to git, bash, coreutils and ripgrep
  inside the image, so the image is `cgr.dev/chainguard/wolfi-base` with those
  packages. dockers_v2 stages binaries as `<os>/<arch>/wright` in the context,
  hence `COPY $TARGETPLATFORM/wright`. Needs GoReleaser ≥ 2.12 and buildx;
  locally use `Containerfile.local` with podman.
- **Alt-screen TUI.** The transcript is a viewport, the composer a textarea and
  the approval prompt an overlay; that layout needs full-height control, so
  the TUI uses the alternate screen. `--plain` (implied by `NO_COLOR`,
  `TERM=dumb`, non-TTY) is the no-alt-screen path for pipes and accessibility.
- **kong, not cobra/fang.** One `CLI` struct with `kong.Parse` is the house
  pattern; `kong.Exit` panics a sentinel so `--version`/`--help` unwind through
  `cli.Main` and tests can drive it.
- **Landlock via re-exec.** Landlock restricts the *calling* process, so the
  backend re-executes the wright binary as `wright __sandbox … -- bash -lc SCRIPT`;
  the hidden subcommand applies `landlock.V5.BestEffort()` and `syscall.Exec`s
  the payload. The sandbox tests use `TestMain` to make the test binary answer
  `__sandbox` too.

## Conventions

stdlib `testing` only, black-box `package x_test`, table-driven, no golden
files, native fuzz tests for every parser (`shellclass`, `policy` rules,
`redact`, `config` decode, `workspace.Resolve`). Every exported identifier is
documented; comments say *why*. `gofumpt`; golangci-lint v2 at 0 issues with
gocyclo ≤ 15 / gocognit ≤ 20 in non-test code (split functions rather than
suppress). Conventional commits; every commit ends with
`Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

## Release recipe

1. `gofumpt -l . && golangci-lint run ./... && go test -race ./...`
2. Libraries first, in this order, each tagged by the user: `llmkit` `v0.x.0`,
   then `llmkit/vertexgrpc`, then `agentkit` `v0.x.0` (bumping its llmkit
   requirement), then `agentkit/mcp`. Then bump the requirements here with
   `GOWORK=off go get github.com/richardwooding/agentkit@v0.x.0 …` and
   `GOWORK=off go mod tidy`.
3. Move `CHANGELOG.md` `[Unreleased]` to the version; update README status.
4. `goreleaser check && goreleaser release --snapshot --clean --skip=publish,docker`
   to see the archives and cask, and
   `podman build -f Containerfile.local -t wright:dev . && podman run --rm wright:dev --version`
   for the image.
5. Tag `vX.Y.Z` and push the tag; `release.yml` runs GoReleaser with
   `GITHUB_TOKEN` (release + ghcr.io) and `HOMEBREW_TAP_GITHUB_TOKEN`
   (richardwooding/homebrew-tap cask).
