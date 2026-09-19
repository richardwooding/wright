# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`wright` is a coding-agent terminal harness (`module github.com/richardwooding/wright`,
Go 1.27, pure Go, no cgo): a Bubble Tea v2 TUI and a headless mode around an
`agentkit` agent, with a permission engine, an OS sandbox, secret redaction and a
tamper-evident audit log between the model and the machine. Generic agent/LLM
plumbing belongs in `agentkit`/`llmkit`; everything coding-specific lives here.
It works end to end today — `wright`, `wright -p`, sessions, audit — and is
pre-1.0 and untagged; MCP and skills are still stubs. User-facing documentation
lives in `README.md` and `docs/index.html`; keep both honest about what exists,
because "no invented capabilities" is part of the product.

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
```

### End-to-end against a fake model (no provider, no network)

`llmkit.ParseModel` routes an unknown bare name to Ollama and the Ollama client
takes its base URL from `OLLAMA_HOST`, so a 40-line fake server is a complete
model provider: serve `GET /api/tags` with one model and `POST /api/chat` with
NDJSON chunks (`{"message":{"role":"assistant","content":"…"},"done":false}`
then `{"done":true,"done_reason":"stop","prompt_eval_count":N,"eval_count":M}`;
add `"tool_calls"` to a chunk to drive the tool path). Point `WRIGHT_DATA_DIR`
and `WRIGHT_CONFIG_DIR` at a scratch directory so a run never touches real
sessions or settings.

```sh
python3 /tmp/fake-ollama.py &                          # throwaway; any server on 127.0.0.1:11434 will do
export OLLAMA_HOST=http://127.0.0.1:11434
export WRIGHT_DATA_DIR=$(mktemp -d) WRIGHT_CONFIG_DIR=$(mktemp -d)

go run ./cmd/wright -p 'say hello' -m fake:latest                     # exit 0, text on stdout
go run ./cmd/wright -p 'list the files' -m fake:latest --output stream-json
go run ./cmd/wright -p 'run curl please' -m fake:latest               # ask → denial naming the --allow rule, exit 3
go run ./cmd/wright sessions list && go run ./cmd/wright audit verify # chain intact
```

The TUI needs a real terminal, so drive it from a pty: `pty.fork()`, write the
prompt plus `\r`, then `\x03` twice within 1.5 s to quit, and assert on what
was written after the alt screen closed — the end-of-run summary
(`steps 1 · tool calls 0 · 39 tok · — · 0s · completed`), the session line and
the token line. `internal/tui/teatest_test.go` covers the same flow in-process;
the pty run is the check that the real binary, the alt screen and the exit
summary agree.

```sh
TERM=xterm-256color python3 /tmp/pty-tui.py go run ./cmd/wright -m fake:latest
```

### Release checks

```sh
goreleaser check
goreleaser release --snapshot --clean --skip=publish,docker   # local release dry run (docker needs buildx; use podman below)

CGO_ENABLED=0 GOOS=linux go build -trimpath -o dist/wright-local ./cmd/wright
podman build -f Containerfile.local -t wright:dev .
podman run --rm wright:dev --version
podman run --rm -v "$PWD:/workspace:Z" wright:dev doctor   # container detected; selected = landlock when the host kernel allows it
```

This module depends on the *tagged* `agentkit` and `llmkit` releases. To develop against an
unreleased sibling checkout, create a temporary `go.work` in `~/Projects/Personal` (not a git
repo) listing `./wright`, `./agentkit`, `./agentkit/mcp`, `./llmkit`; delete it once the tags
exist. Never add `replace` directives.

## Architecture

```
cmd/wright/main.go        kong parse, signal.NotifyContext, exit codes; version/commit/date via ldflags
internal/
  cli/        kong CLI struct + Run methods (run, sessions, models, config, audit, init, doctor, mcp list|add|remove, skills);
              the only importer of app; ExitError{Code} carries headless codes to main; hidden `__sandbox` landlock helper
  app/        composition root: Build = workspaceAndConfig → sandboxing → permissions → modelAndSession → toolsAndEngine;
              Run → headless.Run | Interactive hook (tui wired in cmd/wright); Command hook for /diff /audit /init /trust
              /redaction /mcp /skills /agents; InitProject, LoadEffective, OpenStore, ListSkills/ListMCPServers/Add/RemoveMCPServer
              for the read-only and settings commands; DAG/network tests
  engine/     wraps agentkit: runs, fan-in Event channel, Approver, Asker, Inbox steering, mode/model switch, Compact, Undo
  enginetest/ Scripted core.Chatter + TextResp/CallResp shared by the engine, headless and app tests
  tuiwire/    adapts app.Interactive → tui.Run (Controller/SessionSource shims, "@" file walk); the only package importing both
  tui/        Bubble Tea v2 root model (Controller + Event channel → engine); --plain loop; subpackages
              transcript (block list + render cache), composer (textarea, history, @ and / popups),
              overlay (approval/question/picker/help/todos/confirm/input), markdown (glamour cache),
              diffview (coloured unified diff), fuzzy (substring/subsequence matcher)
  headless/   -p runner: Format text | json | stream-json (Line schema), exit codes 0/1/2/3/4/130, ctx cancel → Engine.Cancel
  tools/      agentkit.Func tools + Describer (resolved policy.Request + Preview); bash cwd/trailer, rg|Go grep, web_fetch robots/rate limit, Clip spill
  skillsdir/  Agent Skills search path (user < .agents < .wright < extraDirs < opt-in .claude) → skills.Set + Problems; Describe for /skills
  mcpclient/  MCP servers: transport (stdio in the sandbox | HTTP via ssrfguard), handshake, tool+annotation listing, trust record, consent; Set{Tools, Servers, Describe, Close}
  agents/     sub-agents as tools: explore (read-only, fast model) + custom .wright/agents/*.md definitions; Build/Toolset/Names/Docs
  policy/     rule grammar, modes, verdict lattice, hard-deny set, grants, child engines
  policy/shellclass/  mvdan.cc/sh AST → per-command class; Unknown/HardDeny; leaf package (interface Workspace)
  sandbox/    Backend: container | bwrap | landlock | seatbelt | none; filtered Env(); __sandbox Helper
  workspace/  roots + extra dirs, symlink-safe Resolve, .gitignore/.wrightignore, secret/protected paths
  redact/     high-confidence secret patterns → "[redacted: <name>…last4]"; line-buffered Writer
  audit/      SHA-256-chained JSONL per session: Open/Write/Read/Verify/Summarize
  snapshot/   content-addressed pre-edit snapshots per run → /undo
  config/     Settings layers (embedded defaults.json < user < project < project.local < env), atomic saves
  trust/      accepted project settings / MCP servers by hash, ~/.config/wright/trust.json
  session/    agentkit FileStore + sessions/meta/<id>.meta.json sidecars: Open/List/Get/Touch/Latest/ExportMarkdown/Delete/Purge
  model/      Choice: flag > WRIGHT_MODEL > settings > credential detection > Ollama probe; Best/Fast/List from the catalog
  cost/       Meter (usage per model → USD via catalog), FormatUSD/FormatTokens; unknown models price as "—"
  prompt/     System(in) → (stable, dynamic); LoadInstructions (AGENTS.md walk, CLAUDE.md fallback); WrapUntrusted, ScanInjection
  git/        exec git: Status (2 s timeout), Diff, IsTracked
  theme/      lipgloss v2 palette (gloam tokens as LightDark pairs) + styles
docs/         gloam Pages site (gloam.css/gloam.js vendored; sync-gloam.sh + gloam-sync.yml keep them current)
```

Import DAG, enforced by `TestImportDAG` in `internal/app` over `go list -deps`:
`tui` never imports `tools`, `sandbox`, `policy/shellclass` (it may name
`policy.Mode`/`GrantOffer`, which `engine.Approval` carries); `tools` never imports `tui`
or `engine`; `policy` never imports `tui`, `engine`, `tools`; `shellclass`
imports nothing internal (it takes a small `Workspace` interface that
`policy.NewShellWorkspace` adapts). `engine` is the seam between agent and UI.
`skillsdir`, `mcpclient` and `agents` are feature packages the app composes:
they never import `tui`, `engine` or `app`, and reach the engine only through
`Options` (`Tools`, `Extra`, `Describe`).
`TestNoUnexpectedNetwork` allowlists the internal packages that may import
`net/http` (`model` for the Ollama loopback probe, `tools` for `web_fetch`, which
is handed the ssrfguard client by `app`, and `mcpclient`, which hands that same
client to HTTP MCP transports).

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
  `Writer` is line-buffered *plus* a bounded private-key lookbehind: a
  multi-line pattern cannot match a single line, so anything spanning lines
  needs holding logic there, not just in `Redact`. The streamed output and
  the whole-buffer output must never disagree about a secret — the user's
  screen is as much a disclosure as the model's context.
- **Redact before `Clip`.** `Clip` writes the full text to the spill file and
  names that path to the model, so any tool that spills must redact first
  (`bash`, `web_fetch`). Redacting the clipped result only cleans the excerpt
  and leaves the secret on disk.
- **A described path must be the path that runs.** `bash` executes with
  `spec.Dir = Cwd.Get()`, which `cd` moves, so `describeBash` wraps
  `policy.NewShellWorkspace` in `cwdWorkspace` to resolve relative words
  against that directory. Anything that classifies a command has to use the
  same base the command will use, or the verdict and the approval preview
  describe a different file from the one that is opened.
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
- **Project settings are inert until trusted.** `app.effectiveSettings`
  rebuilds the layers itself (defaults < user < project < project.local <
  env) and drops the project's `allow`, `additionalDirectories`, `passEnv`
  and `mcpServers` unless `trust.json` holds the hash of the current
  `.wright/settings.json`; project `ask`/`deny` always apply. The same split
  feeds `policy.New` (project allow rules only when trusted). `wright init`
  trusts the file it writes; `/trust` accepts an existing one, effective from
  the next session. `config show` prints the gated view.
- **Headless exit 3 comes from the tool result.** With `Options.Headless`
  the engine answers every Ask verdict with a denial containing
  `engine.HeadlessDenialMarker`; `headless.Run` scans `KindToolResult` text
  for it. Budget stops (`max_steps`, `deadline`, …) arrive with `Finish.Err`
  set too, so `exitCode` checks them before the generic error case.
- **`app` never imports `tui`.** `app.Run` takes an `Interactive` func;
  `cmd/wright` passes the TUI's entry point (nil today → `ErrNoInteractive`).
  Tools reach the engine through `lateEngine` (Asker, OnRedacted, todo
  push) because the toolset is built before `engine.New` needs it. The
  `cli.ExitError` *type* carries exit codes, so the code constant is
  `cli.ExitFailure`, not `ExitError`.
- **MCP annotations are claims, never permissions.** `readOnlyHint`,
  `destructiveHint` and `openWorldHint` come from the server. They are shown
  in the proposal and the approval prompt, and `Request.ReadOnly` only
  decides whether *plan mode* asks or refuses; nothing else reads them. A
  server is identified by hash (binary, args, URL, tool list) in
  `trust.json`, so a changed tool list re-prompts — and the tool list is
  hashed under the server's *own* names, which is also what
  `mcp:<server>:<tool>` addresses. Resolve a registered name through
  `Set.Describe`, not by splitting on "_": a server name may contain one.
- **Children never widen permissions.** A sub-agent is a tool; the app gives
  it `Engine.Middleware()` (via `lateEngine.middleware`, resolved per call
  because sub-agents are built before the engine exists), and
  `Engine.Approve` evaluates `Depth > 0` against `policy.Engine.Child`, which
  clamps bypass to default and refuses grants. Calling a sub-agent is allowed
  by a rule the app synthesises from its name (`subAgentRules`) because the
  call itself has no effect — everything it then does is evaluated again one
  level deeper. Giving an agent a tool in its `tools:` list is not permission
  to use it.
- **Skills are text, not capability.** `skills.Use` adds the catalog to the
  prompt and registers `skill`/`skill_file`, which only return text and
  bundled files (hence their place in `policy.otherTools`). A script a skill
  ships runs only through `bash`, where the usual rules and sandbox apply.
- **The stable prompt must stay stable.** `prompt.System` returns two strings:
  the stable half is sent with `WithInstructions` + `WithCache` and must be
  byte-identical across turns and sessions (no date, cwd, model, mode, git);
  everything that varies goes in the dynamic environment block.
  `TestSystemStableIsIdenticalAcrossTurns` pins it — put new facts in
  `environment`, not in the constants.
- **Instruction files are quoted, never ratified.** `prompt.System` renders
  every `Instruction` inside `<instructions source=… scope=…>` after the
  framing that says a `scope="project"` file came from the repository and
  cannot override the operating constraints or the permission policy;
  `ScopeUser` (the user's `$XDG_CONFIG_HOME/wright/AGENTS.md`) is the only
  scope that is the user's own, and the zero value frames as project.
  `LoadInstructions` runs `ScanInjection` over each body and attaches the
  signals, which become the block's `warning` attribute and an app warning
  naming the file — never load one silently. Both `</instructions` and
  `<instructions` are escaped in bodies, so a file can neither close its
  block nor forge an opening tag with a scope of its choosing.
- **Session sidecars live in `sessions/meta/`.** `agentkit.FileStore.List`
  reads every `*.json`/`*.jsonl` in its directory as a transcript (and skips
  subdirectories), so `<id>.meta.json` beside the transcript would be parsed
  as a legacy session. `Delete` also removes `snapshots/<id>` and
  `audit/<id>.jsonl`; IDs go through `ValidID` before touching any path.
- **Model names are provider-qualified when needed.** `llmkit.ParseModel`
  routes `org/model` names to Hugging Face and unknown bare names to Ollama,
  so `model.Best`/`Fast` emit `groq/openai/gpt-oss-120b`, `openrouter/…` via
  `qualify`; `catalog.Lookup` strips the prefix again. `Detect` never reads
  the process environment or the network itself — `env` and `probeOllama` are
  injected — and `ProbeOllama` refuses non-loopback hosts unless `OLLAMA_HOST`
  is set.

### TUI

- **Alt-screen, one root model.** `tui.Model` owns a transcript viewport, the
  composer, at most one overlay and the status bar; `Run` returns the
  end-of-run summary to print after the alt screen is gone. `--plain`
  (`Options.Plain`, implied by `NO_COLOR`/`TERM=dumb`/non-TTY) never starts
  Bubble Tea: `plain.go` prints events as lines and reads answers from stdin,
  denying any prompt that is still open when stdin closes.
- **The engine is the only source of truth.** The UI reaches the engine
  through `Controller` and reads it through `engine.Event`; nothing in `tui`
  decides permissions. Overlays cannot mutate the root model, so pickers and
  the confirm prompt hand results back as messages (`pickedMsg`); approval and
  question overlays call `Controller.Reply/Answer` directly.
- **33 ms coalescing.** `KindText`/`KindReasoning` deltas only append to the
  live block and arm a single in-flight `flushMsg` tick (`flushInterval`); the
  live block is re-rendered on the flush, so a fast stream costs one glamour
  render per frame, not per token. Every other event refreshes at once.
- **Render cache invalidation.** `transcript.Model` caches each block's lines;
  a delta invalidates only the live block (`Invalidate`), a finished block is
  never re-rendered, and only `Lines(width)` with a new width, `SetTheme`
  (after `tea.BackgroundColorMsg`) or `SetShowReasoning` invalidate
  everything. `markdown.Renderer` caches one glamour renderer per
  `(width, isDark)`. Break either rule and a long transcript re-renders on
  every keystroke.
- **UI honesty.** Every status is glyph + word (`✓ ok`, `✗ denied`,
  `⛔ bypass`, `sandbox off`), never colour alone; the approval prompt focuses
  deny for `SeverityDestructive`, offers "allow…" only when the engine
  produced `Offers`, shows each offer's exact rule text and scope, validates
  edited arguments with `json.Valid`, and `esc` is deny. Bypass is reachable
  only through the typed-word confirm and still goes through
  `Controller.SetMode`, which the policy engine refuses without the flag.
- **charm v2 gotchas.** Import paths are `charm.land/...`; `lipgloss.Style.Width`
  is the whole block including border and padding; `textarea` handles
  `tea.PasteMsg` itself; `tea.KeyPressMsg.String()` spells keys as
  `shift+tab`, `ctrl+j`, `pgup`, `pgdown`, `alt+enter`.

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
   `go get github.com/richardwooding/agentkit@v0.x.0 …` and `go mod tidy`.
3. Move `CHANGELOG.md` `[Unreleased]` to the version; update README status.
4. `goreleaser check && goreleaser release --snapshot --clean --skip=publish,docker`
   to see the archives and cask, and
   `podman build -f Containerfile.local -t wright:dev . && podman run --rm wright:dev --version`
   for the image.
5. Tag `vX.Y.Z` and push the tag; `release.yml` runs GoReleaser with
   `GITHUB_TOKEN` (release + ghcr.io) and `HOMEBREW_TAP_GITHUB_TOKEN`
   (richardwooding/homebrew-tap cask).
