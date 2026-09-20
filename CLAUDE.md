# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`wright` is a coding-agent terminal harness (`module github.com/richardwooding/wright`,
Go 1.27, pure Go, no cgo): a Bubble Tea v2 TUI and a headless mode around an
`agentkit` agent, with a permission engine, an OS sandbox, secret redaction and a
tamper-evident audit log between the model and the machine. Generic agent/LLM
plumbing belongs in `agentkit`/`llmkit`; everything coding-specific lives here.
It works end to end today — `wright`, `wright -p`, sessions, audit, MCP,
skills, sub-agents — and is pre-1.0, first tagged as v0.1.0. User-facing documentation
lives in `README.md` and `docs/index.html`; keep both honest about what exists,
because "no invented capabilities" is part of the product.

## Commands

```sh
go build ./... && go vet ./... && go fix -diff ./...     # CI fails on any go fix diff; run it before pushing
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
podman run --rm --userns=keep-id -v "$PWD:/workspace:Z" wright:dev doctor   # container detected; selected = landlock
```

This module depends on the *tagged* `agentkit` and `llmkit` releases. To develop against an
unreleased sibling checkout, create a temporary `go.work` in `~/Projects/Personal` (not a git
repo) listing `./wright`, `./agentkit`, `./agentkit/mcp`, `./llmkit`; delete it once the tags
exist. Never add `replace` directives.

## Architecture

```
cmd/wright/main.go        kong parse, signal.NotifyContext, exit codes; version/commit/date via ldflags
internal/
  cli/        kong CLI struct + Run methods (run, sessions, models, config, audit, init, doctor, mcp list|add|remove, skills,
              trust list|accept|forget);
              the only importer of app; ExitError{Code} carries headless codes to main; hidden `__sandbox` landlock helper
  app/        composition root: Build = workspaceAndConfig → sandboxing → permissions → modelAndSession → toolsAndEngine;
              Run → headless.Run | Interactive hook (tui wired in cmd/wright); Command hook for /diff /audit /init /trust
              /redaction /mcp /skills /agents; InitProject, LoadEffective, OpenStore, ListSkills/ListMCPServers/Add/RemoveMCPServer
              for the read-only and settings commands; DAG/network tests
  engine/     wraps agentkit: runs, fan-in Event channel, Approver, Asker, Inbox steering, mode/model switch, Compact, Undo;
              grantMiddleware puts the approved call's sandbox.Grant (network + writable tool prefixes) on its context
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
  sandbox/    Backend: container | bwrap | landlock | seatbelt | none; filtered Env(); __sandbox Helper;
              Grant/WithGrant + ToolPrefixes: the per-call widening an approval earns
  workspace/  roots + extra dirs, symlink-safe Resolve, .gitignore/.wrightignore, secret/protected paths
  redact/     high-confidence secret patterns → "[redacted: <name>…last4]"; line-buffered Writer
  audit/      SHA-256-chained JSONL per session: Open/Write/Read/Verify/Summarize
  snapshot/   content-addressed pre-edit snapshots per run → /undo
  config/     Settings layers (embedded defaults.json < user < project < project.local < env), atomic saves
  trust/      accepted workspaces (the directory itself) and project settings / MCP servers by hash, ~/.config/wright/trust.json
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
- **An option or an assignment can be a command.** Every name in a table's
  safe-read set (`gitSafeRead`, the readers) is auto-allowed with *no*
  prompt, so any option of it that names a program to run (`git bisect run`,
  `git grep --open-files-in-pager`/`-O`, `difftool --extcmd`, `sort
  --compress-program`), a file to read (`git blame --contents`) or a file to
  write (`git show|log|diff --output`, `git config --file`, `git archive -o`)
  is arbitrary execution or disclosure behind a name that reads as harmless.
  Before adding a command there, read its options for `--*-pager`,
  `--*-command`, `--*-tool`, `--*-filter`, `--output`, `--contents`, `--file`
  and `--no-index`; an option that runs a program is `Privilege` *and*
  opaque, and one that names a path is a declared read or write so the
  secret/protected floors and the containment checks see it. The same holds
  for an environment prefix: an assignment lands in `Command.Env`, never in
  the argv an allow rule matches, so `GOFLAGS=-toolexec=./x go build ./...`
  rode the builtin `bash(go build *)` rule until `dangerousVar` learned the
  build variables. They are exported as `shellclass.InjectingEnvVars` —
  `sandbox.envStrip` covers the same ground and should read that list rather
  than keep a second copy.
- **A git command can be pointed at another repository.** `-C`,
  `--git-dir` and `--work-tree` bring a foreign config (aliases, hooks path,
  filters) and change what every relative path in the command means, so a
  value outside the workspace is opaque. The global options taint the
  subcommand's result instead of replacing it — returning early there would
  drop the hard deny `git push --force origin main` raises.
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
- **The workspace-trust baseline lives in the mode table, never in a rule.**
  `Engine.TrustWorkspace` makes an ordinary in-workspace edit *allow* instead
  of *ask*, and it is applied in `modeWrite`'s default branch
  (`modeWriteDefault`) on purpose. Writing it as an allow rule would be
  simpler and would silently widen it: allow rules are consulted *before* the
  mode table, and `containment()` drops the ask half for the write tools
  (`if ev.kind != kindBash { ask = "" }`) because `modeWrite` re-derives it
  through `ev.outside(...)` — so a rule returns with the sensitive-file
  (`*.tfvars`) and ignored-file asks never evaluated. The mode-table grant
  alone is not enough either: the builtin ask rules
  `write_file($WORKSPACE/**)`/`edit_file($WORKSPACE/**)` answer one step
  earlier, so `matchDecision` skips *exactly those* for a request
  `trustBaseline()` covers (a trusted workspace, a write tool with at least
  one declared write, default mode). `supersededByTrust` recognises them by
  `Source == SourceBuiltin`, a write tool and `Pattern == "$WORKSPACE/**"` —
  the mode table's default written as a rule — so a *narrower* builtin ask
  rule added later still decides, and a changed default pattern makes trust
  stop granting rather than start over-granting. Both halves are load-bearing; reverting either one fails
  `TestEvaluateTable`. `multi_edit` is in `writeTools` with no builtin ask
  rule, so it reaches `modeWrite` by the other route and is pinned
  separately. The verdict names `policy.SourceTrust` in its `Reason` so the
  prompt and the audit log can say which decision granted it. Every floor
  still runs first: hard-deny, `.wrightignore`, deny rules, explicit ask
  rules, containment, outside-the-workspace, sensitive files, ignored files,
  and plan mode, which never consults allow rules at all.
- **The builtin write asks are the mode table's default, written as rules.**
  `write_file($WORKSPACE/**)`, `edit_file($WORKSPACE/**)` and
  `multi_edit($WORKSPACE/**)` say what *default mode* does, but they are
  matched one step *above* `modeTable`, so any mode whose own answer differs
  is pre-empted by them and never runs. That is not theoretical: auto-edit
  mode asked for every edit for the whole life of the project, while the
  README said it allowed them, and `TestEvaluateTable`'s auto-edit rows
  passed because they omitted the builtin layer. `eval.modeAnswersWrites`
  is the list of modes that supersede those three rules (auto-edit always,
  default mode when the workspace is trusted); anything that changes
  `modeWrite` has to add a row **with `layers: {builtin}`**, because a row
  without it is testing a program that does not exist.
- **Builtin rules and `config/defaults.json` must match.** `policy/builtin.go`
  and the embedded JSON are the same list; `TestBuiltinMatchesConfigDefaults`
  fails if they drift.
- **Bypass is never inherited or settable.** `Engine.SetMode(ModeBypass)`
  returns `ErrBypassNotSettable`; `Child` clamps bypass to default; children
  cannot `Grant`. Only `--bypass-permissions` at construction enables it.
- **Symlink-safe paths.** `workspace.Resolve` evaluates symlinks on the deepest
  existing ancestor and appends the rest, so `ws/link/x` where `link → ~/.ssh`
  resolves outside. Every path in a `policy.Request` must have gone through it.
  **Anything that matches a path against a fixed set must therefore know the
  *resolved* spelling.** `workspace.Open` resolves the protected locations
  once and `IsProtected` checks the literal and the resolved form: on macOS
  `/etc`, `/var` and `/tmp` are symlinks into `/private`, so a literal-only
  `/etc/**` matched nothing a request could ever carry — `/etc/passwd`
  arrived as `/private/etc/passwd`, read as merely "outside the workspace",
  and bypass mode allowed it. `$HOME` can sit under a link and `~/.ssh` or
  `~/.gitconfig` can *be* one (dotfiles repositories), so the home entries
  get the same treatment. Tests must compute the expectation with
  `filepath.EvalSymlinks` at run time; a hardcoded `/private/...` asserts
  nothing on Linux.
- **The sandbox hides `$HOME`.** bwrap mounts a tmpfs over the home directory
  *before* re-binding workspace roots and caches (order matters). The landlock
  helper lists system directories explicitly (`systemRO`) — never
  `RODirs("/")`, which would expose `/home`. Device nodes go through
  `RWFiles`, not `RWDirs` (Landlock rejects directory rights on files).
- **A read-write root is not writable all the way down.** `.git/hooks`,
  `.git/config`, `.git/config.worktree`, a `.git` *file* and `.wright` stay
  read-only inside the sandbox (`sandbox.ProtectedIn`), and wright's own
  config/state dirs (`sandbox.UserDirs`) are never bound read-write — a hook
  written inside runs *outside*, on the next commit, and `.wright` decides
  the next session's permissions. The rest of `.git` stays writable so `git
  add`/`git commit` still work. bwrap `--ro-bind`s them *after* the root's
  `--bind` (last operation wins) and tmpfs-masks the user dirs; seatbelt
  denies after the allows (last SBPL rule wins). **Landlock rules are
  additive** — the kernel unions every rule matching an ancestor, so a
  subtree can never be subtracted from an RW rule — so landlock grants the
  root read-write as normal and the helper *bind-mounts* the protected paths
  read-only instead. Withholding write on the root to compensate is not an
  option: an agent that cannot create a file in the repository root is not
  an agent. `EnsureProtected` creates the protected paths that do not exist
  yet (in the workspace roots only, and never a `.git` of its own
  invention), because a path that is absent cannot be bound and the payload
  would just `mkdir` it. The policy layer already denies *declared* writes
  there (`workspace.IsProtected`); this is the layer for writes it cannot
  see.
- **Approval grants what the command needs.** This is a deliberate
  security-posture decision, not an accident to tidy away. When
  `shellclass` says a bash command needs the network, the *ordinary* "allow"
  grants the network for that call (`engine.callGrant` → `verdict.Network`),
  and when it says the command installs software (`Analysis.Installs`) the
  approval also mounts `sandbox.ToolPrefixes()` read-write **for that one
  call**. The prompt states both before the answer and names the exact
  directories: consent has to be to something specific. There is no separate
  "allow with network" option, and adding one back would recreate the bug —
  the user allowed `brew info fpc`, got a call with no network, and read
  `curl: (7) Could not connect`. A harness that cannot do ordinary work is
  not secure, it is broken. What must *not* change with it:
  the model's own `network: true` argument never self-grants (it turns an
  allow into an ask, and `Engine.allowed` rewrites the argument to the
  verdict for every bash call); a *persisted* rule still needs an explicit
  `+net`, which is what makes a saved grant explicit; and the write grant
  comes only from an interactive approval — never from an allow rule, never
  from bypass mode, and never in the base `Spec`. The grant reaches the tool
  through `sandbox.WithGrant` on the call's context, applied by
  `Engine.grantMiddleware` directly inside `agentkit.ApproveWith`, because
  that middleware runs the tool with the context it already had: an
  `Approver` cannot add to it, so the decision and the context are joined by
  a per-call entry (`Engine.grants`, keyed by call ID + final arguments) that
  the middleware pops. `bash` copies the spec per call and must *clone*
  `ReadWrite` before appending — `Deps.SandboxSpec` is shared, so appending
  in place would hand the next command the last one's prefixes.
- **A package-manager verb that uses the network is not a safe read.** A
  command classified `SafeRead` is auto-allowed with no prompt *and* no
  network, so misclassifying one is not "slightly wrong", it is a command
  that can never work and a user who is never asked. `brew info`, `deps`,
  `outdated`, `search` and `doctor` all contact the Homebrew API or a tap's
  remote; only `list`/`ls`, `config`, `leaves` and the `--prefix`/`--version`
  options are local. The same question has to be asked of every manager
  before adding a verb to a safe set: `npm doctor`/`ping` contact the
  registry, `pip list --outdated` and `gem list --remote` query the index,
  and `dnf`/`yum`/`snap`/`flatpak` refresh remote metadata even for queries,
  while `apt`, `pacman`, `apk`, `zypper`, `rpm` and `port` answer from the
  index already on disk. Note also that `verbs.lookup` and `first()` read the
  first *positional* word, so an option-spelled verb (`brew --prefix`,
  `pacman -Ss`) never matches a map key — it has to be matched with
  `hasFlag`.
- **The landlock helper's namespaces are the confinement.** The backend
  re-execs it with `CLONE_NEWUSER|CLONE_NEWNET`, `Unshareflags:
  CLONE_NEWNS`, an identity uid/gid map and ambient `CAP_SYS_ADMIN`
  (`nsAttr`): `RestrictNet` covers TCP bind/connect only, so the network
  namespace is what makes "no network" true, and the mount namespace is what
  carries the read-only binds. Ambient is the only capability kind that
  survives the `execve` into the helper — a user namespace grants everything
  at clone time and `execve` by a non-root uid drops it all. The helper
  mounts, then `dropCapabilities`, then Landlock, then `syscall.Exec`, all on
  one `runtime.LockOSThread` thread, because capabilities are per-thread and
  `execve` takes the calling thread's. **Never replace a sandboxed command's
  `SysProcAttr`** — add to it (`tools.setProcessGroup` once replaced it and
  silently cost the backend both namespaces); the helper now compares its
  namespaces with `Helper.ParentNS` and refuses to run if they are the
  parent's. A remount can still fail where the filesystem's mount is locked
  by another user namespace — a container runtime's volume — so the probe
  (`__sandbox --probe --probe-dir <root>`) measures the *workspace*, and
  `WarningsFor` says plainly that those paths rest on the permission layer
  alone. A test that asserts what the namespaces carry has to gate on those
  same probes — `LandlockNamespaces` for the namespaces, `SpecWarnings` for
  the read-only binds — and skip with the backend's own reason where it
  cannot enforce, exactly as the bwrap tests skip without bwrap. Never gate
  on a CI environment variable: that also silences a machine that *can*
  enforce and does not. Name resolution through a local resolver's unix socket survives
  under both backends; that is a property of the host, not of the sandbox.
- **`sandbox.Env` is an allowlist with a hard strip.** Passthrough from trusted
  settings can add names, but nothing matching the strip regexes
  (`_KEY|_TOKEN|_SECRET|_PASSWORD`, `AWS_`, provider prefixes…) ever reaches a
  sandboxed command, even if passed through. A variable that injects code
  into an allowed command is as good as an allowed command, so `GOFLAGS`
  (`-toolexec`), `NODE_OPTIONS` (`--require`), `LD_PRELOAD`, `BASH_ENV`,
  `GOPROXY` (credentials) and the checksum switches are stripped, and the Go
  variables are named one by one rather than globbed as `GO*`.
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
- **Sandboxed commands are `bash -c`.** Never `-lc`: a login shell sources
  `/etc/profile`, `/etc/profile.d/*` and the user's `~/.bash_profile` (real
  on the `none` backend), which can undo the filtered environment. The
  environment is explicit and carries `PATH`.
- **`web_fetch` checks the host itself.** ssrfguard validates every dial, but
  the tool also refuses loopback, unspecified, link-local, RFC1918, CGNAT and
  metadata hosts in `parseFetchURL` — including the integer, hex, octal and
  IPv4-mapped spellings — so a URL that can never work is refused before the
  request and before the approval prompt shows it. Only `Deps.AllowLocalFetch`
  (tests serving from 127.0.0.1) lifts it. robots.txt product tokens match
  exactly, never by prefix.
- **Audit lines are immutable.** `audit.Log.Write` sets `Seq` and `Prev` (the
  SHA-256 of the previous *line*), redacts `Text`/`Args`, caps `Args` at 4 KiB
  and records `ArgsSHA256` of the full value. Never log environment variables
  or request bodies. `Verify` walks the chain, which only proves the log is
  self-consistent: the chain runs backwards, so a truncated log — or one
  whose lines were edited and whose `prev`s were recomputed — verifies
  perfectly. The witness is outside the file: every `Write` records the new
  head (seq + line hash) through `audit.Anchors` in
  `Paths.AuditAnchorDir()`, mode 0600, and `VerifyAnchored` reports
  `ErrTruncated`, `ErrForged` and `ErrNoAnchor` as different things. Never
  put the anchor beside the log, and never treat a missing anchor as
  success — deleting it is the first step of the attack.
  A `decision` line records what *happened*, which is not the verdict: on
  both user branches of `Engine.ask` the verdict is still `Ask` (Ask is what
  raised the prompt), so `auditDecision` takes the outcome from the user's
  answer, and `granted_network`/`granted_writable` record what allowing that
  one call handed it — `grant` is a *saved rule*, a different thing.
  `Summary.Asked` therefore counts `by: "user"` lines rather than the `ask`
  outcome, so it still means "prompts shown". The headless branch denies
  without prompting, so it records `deny` too — a run nobody watched is the
  one whose log has to be true.
- **Config lists accumulate.** `config.Merge` appends+dedupes `allow/ask/deny`
  (and other slices) and overrides scalars only when non-zero; booleans that
  default to true are `*bool` so a later layer can turn them off.
- **Doctor prints credential *names* only.** Never print an environment value.
- **Project settings are inert until trusted — both files.**
  `app.ProjectHash` hashes `.wright/settings.json` *and*
  `.wright/settings.local.json` as a unit, so a repository shipping only the
  local file cannot be trusted by default, and `resolveTrust` falls through to
  the hash check as soon as either exists. `app.effectiveSettings` rebuilds
  the layers itself (defaults < user < project < project.local < env) and
  runs *both* project layers through `tighteningOnly` unless `trust.json`
  holds the current hash: what survives untrusted is `ask`, `deny` and
  `redaction: true`; what waits for trust is `allow`, `mode`,
  `additionalDirectories`, `passEnv`, `mcpServers`, every `sandbox.*` key,
  `model`, `skills`, `git.trailer`, `instructions.files` and
  `redaction: false`. Anything added to `Settings` that could *widen* has to
  be kept out of `tighteningOnly` and named in `untrustedNote` — the note is
  read as an assurance, so it must list everything that was dropped. The same
  split feeds `policy.New` (project *and* project-local allow rules only when
  trusted). A project settings file that does not *parse* is a warning and a
  dropped layer, not a fatal error: it arrived with the repository, so making
  it fatal would let any checkout stop wright from starting in that
  directory. The user's own `config.json` stays fatal — it is theirs to fix —
  and `config.Decode` names the line instead of returning a bare `EOF`.
  `wright init` trusts the files it writes; a persisted grant
  rewrites `settings.local.json` and re-records the hash, but only for an
  already-trusted project. `/trust` accepts an existing pair, effective from
  the next session. `config show` prints the gated view. A project is keyed
  in `trust.json` by its absolute, symlink-resolved root
  (`trust.normalizeRoot`), never by the spelling a caller happens to hold:
  accepting under one spelling and checking under another is how project
  trust silently stopped working on macOS.
- **Trusting a directory and trusting its settings are two questions.**
  `trust.Record` holds them in separate fields (`SettingsHash`/`Accepted` and
  `WorkspaceAccepted`) and every accessor preserves the other, so editing
  `.wright/settings.json` cannot revoke the workspace and accepting the
  workspace cannot vouch for settings nobody read. An old record has no
  `workspaceAccepted`, which reads as "never asked" — the right answer on
  upgrade. `app.resolveTrust` answers both with at most one `Confirm`
  (fused when both are pending, so a first run asks once), *before*
  `sandboxing` and `permissions`; declining the workspace returns
  `ErrWorkspaceNotTrusted`, which `run.go` turns into `ExitTrustDeclined`
  (5) with no session created. **A headless run (`b.headless()`: `-p` or no
  terminal) is never asked, never exits over trust and never gets the
  baseline**, whatever `trust.json` holds — the guard is one `&&
  !b.headless()` in `resolveTrust`, and removing it lets a scripted
  `edit_file` run with no approval at all. `--trust` (`RunOptions.
  TrustWorkspace`) answers the workspace question only; `wright trust
  accept|forget` is the out-of-band way in and back out, since nothing
  called `ForgetProject` before it.
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
- **A background job outlives its call, so it must not outlive the session.**
  `bash` with `background` returns at once, and the job keeps whatever its
  approval granted it — the network, an install's writable prefixes — for as
  long as it runs. Its context is therefore `context.WithoutCancel` of the
  call's (agentkit cancels that the moment the tool returns) but still
  cancellable, and `JobSet.Close` — wired into `Built.Close` — kills every
  one. A test that asserts a job is "not running" proves less than it looks:
  `cmd.Wait` returns once the direct child is reaped and `WaitDelay` has
  elapsed, so the process it spawned can still be alive. Ask the operating
  system (`syscall.Kill(pid, 0)`), as `TestJobSetCloseKillsTheProcessTree`
  does.
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
- **Tool call IDs are not session-unique.** llmkit synthesises `call_1`,
  `call_2` … per response for Ollama and Gemini, so the same ID comes back
  every turn. Anything joining the transcript to another record — today
  `ExportMarkdown` against the audit log — must walk both forward together
  (both are append-only and chronological) and consume each entry at most
  once, matching on `(call ID, tool name)`. A map keyed by call ID gives
  every turn's `call_1` the first turn's decision. That join also drops
  `Depth > 0` decisions: a sub-agent's calls are not in this transcript and
  its synthesised IDs collide with the main agent's, so a sub-agent decision
  left in the stream is handed to the next real call. And the audit log is
  evidence *about* a session, not part of it — it can be absent
  (`Options.Audit` was nil, `Delete` removed it) or stop at a malformed line,
  since `audit.Read` yields the error and ends — so the export keeps what it
  read, says so in a note, and never fails because of it.
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
  deny for `SeverityDestructive`, says in the "allow" label *and* in the facts
  what allowing hands over (`Approval.Grants`: network, and the exact
  directories an install will be able to write), offers "allow…" only when the
  engine produced `Offers`, shows each offer's exact rule text and scope, validates
  edited arguments with `json.Valid`, and `esc` is deny. Bypass is reachable
  only through the typed-word confirm and still goes through
  `Controller.SetMode`, which the policy engine refuses without the flag.
- **The row budget is fixed and exact.** `View` renders
  transcript + optional queued strip + rule + composer + rule + status bar, and
  the total must equal the terminal height: `viewportHeight` is
  `m.height - 3 - comp.Height() - stripH` (the 3 is the status bar and the two
  rules around the composer), and the composer's cursor is offset by
  `vpH + stripH + 1` for the rule above it. Change the chrome and all three
  numbers move together; `TestViewFitsTerminalHeight` pins them at several
  heights, with and without the strip and an overlay. The transcript clamps to
  one row below ~8 rows, so the chrome overflows a terminal that short by
  design. An overlay is `clampRows`-ed to the transcript's height because it
  has a minimum size (border + title) and `lipgloss.Place` does not truncate.
  The rules are `strings.Repeat("─", m.width)` in `theme.Rule`, never a
  lipgloss bordered box, which would add side columns.
- **The mouse is off by default.** `v.MouseMode` is left at
  `tea.MouseModeNone` unless the user toggles it (`alt+m` or `/mouse`), because
  cell-motion tracking takes click-and-drag away from the terminal and kills
  text selection and copy — for the sake of one feature, wheel-scrolling the
  transcript. `shift+up`/`shift+down` are the keyboard replacement (`ctrl+u` is
  the composer's, and `pgup`/`pgdn` page). `MouseMode` is per-frame and the
  renderer emits the reset sequences, so the toggle needs nothing else.
  `m.vp.MouseWheelEnabled` is orthogonal: it only matters once a
  `MouseWheelMsg` can arrive at all.
- **The window title carries run state.** `● wright — <dir>` while `m.running`,
  plain when idle, and set on the pre-size placeholder frame too. `--plain`
  never starts Bubble Tea and sets no title.
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
  backend re-executes the wright binary as `wright __sandbox … -- bash -c SCRIPT`;
  the hidden subcommand applies `landlock.V5.BestEffort()` and `syscall.Exec`s
  the payload. The namespaces are created by the *backend*, as clone flags on
  that re-exec, not by the helper: `unshare(CLONE_NEWUSER)` returns EINVAL in
  a multi-threaded process, and every Go program is one. Running `wright
  __sandbox` by hand therefore gets the filesystem rules but no namespaces
  and no read-only binds. The sandbox tests use `TestMain` to make the test
  binary answer `__sandbox` too — and it must exit 0 when `Exec` returns,
  which only `--probe` does.

## Conventions

stdlib `testing` only, black-box `package x_test`, table-driven, no golden
files, native fuzz tests for every parser (`shellclass`, `policy` rules,
`redact`, `config` decode, `workspace.Resolve`). Every exported identifier is
documented; comments say *why*. `gofumpt`; golangci-lint v2 at 0 issues with
gocyclo ≤ 15 / gocognit ≤ 20 in non-test code (split functions rather than
suppress). Conventional commits; a commit made by an assistant ends with its own
`Co-Authored-By:` trailer (the model in use, not a fixed name).

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
