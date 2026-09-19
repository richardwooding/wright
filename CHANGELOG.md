# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

First working version of wright: an interactive TUI and a headless runner
around an `agentkit` agent, with a permission engine, an OS sandbox, secret
redaction and a tamper-evident audit log between the model and the machine.

### Added

**Interface**

- Bubble Tea v2 TUI (`internal/tui`): transcript viewport with a per-block
  render cache, markdown through a cached glamour renderer, collapsible tool
  cards with unified diffs, an approval overlay that focuses *deny* for
  destructive requests and shows the exact rule each "allow…" offer would
  write, a composer with history, `@` file completion and `/` command
  completion, and a status bar carrying model, mode, context %, tokens, cost,
  sandbox, git and session.
- Slash commands `/help /clear /compact /cost /diff /mode /model /sessions
  /resume /export /undo /init /mcp /skills /todos /audit /redaction
  /reasoning /trust /plain /quit`, and keys `enter`, `shift+enter`,
  `alt+enter`, `ctrl+j`, `esc`, `ctrl+c ctrl+c`, `ctrl+d`, `ctrl+o`,
  `ctrl+t`, `shift+tab`, `pgup`/`pgdn`, `ctrl+u`, `ctrl+l`.
- `--plain` path (implied by `NO_COLOR`, `TERM=dumb` or a non-TTY): no
  alternate screen, events as lines, numbered prompts on stdin, and any
  prompt still open when stdin closes is denied.
- `internal/headless`: `wright -p` with `text`, `json` and `stream-json`
  output, a documented `Line` schema, exit codes 0/1/2/3/4/130, stdin fenced
  as `<stdin>`, and interrupt handling through the engine.
- `internal/cli` (kong): `run`, `sessions list|show|export|delete|purge`,
  `models`, `config show|paths`, `audit show|verify`, `init`, `doctor`, and
  the hidden `__sandbox` Landlock helper; `ExitError{Code}` carries headless
  exit codes to the process.

**Agent**

- `internal/engine`: the seam between `agentkit` and the UI — runs, a fan-in
  event channel, the policy-backed `Approver`, the tools' `Asker`, inbox
  steering while a run is in flight, mode and model switching, compaction,
  `/undo`, cost and context accounting.
- `internal/tools`: `read_file`, `write_file`, `edit_file`, `glob`, `grep`
  (`rg --json` when present, Go regexp otherwise), `list_dir`, `bash`,
  `web_fetch`, `web_search`, `todo_write`, `ask_user` — each with a
  `Describer` that resolves the policy request and a preview (diff, or
  command plus class summary). `bash` tracks cwd across calls, streams output
  as progress, appends the attribution trailer to `git commit`, and clips
  large output to a spill file.
- `internal/agents`: the built-in read-only `explore` sub-agent and custom
  agents from `.wright/agents/*.md` (frontmatter `name`, `description`,
  `tools`, `model`, `read-only`; the body is the instructions). Sub-agents run
  through the engine's own middleware chain and are approved at depth 1 by a
  child policy engine, so a child can never widen what the parent may do.
  Their events carry a depth and render indented. `/agents`.
- `internal/skillsdir`: Agent Skills discovery — merges
  `~/.config/wright/skills`, `<ws>/.agents/skills`, `<ws>/.wright/skills`,
  `skills.extraDirs` and (opt-in) `<ws>/.claude/skills`, later directories
  winning; a malformed `SKILL.md` is reported as a problem, not a failure.
  A skill's scripts still run only through `bash`, under policy.
  `wright skills` and `/skills`.
- `internal/prompt`: the system prompt split into a byte-stable, cacheable
  half (identity, operating principles, codebase rules, tool guidance, an
  ethics section and project instructions) and a dynamic environment block;
  `AGENTS.md` discovery with a `.wright/instructions.md` companion and a
  consented `CLAUDE.md` fallback; `WrapUntrusted` and `ScanInjection`.
- `internal/model`: model choice from flag > `WRIGHT_MODEL` > settings >
  credential detection (Anthropic → OpenAI → Vertex → xAI → DeepSeek →
  OpenRouter → Groq) > a loopback Ollama probe; `Best`, `Fast` and `List`
  from the llmkit catalog.
- `internal/cost`: usage per model priced through the catalog; unknown models
  render as `—` rather than a guess.

**Safety**

- `internal/policy`: rule grammar (path globs, bash argv prefixes, `re:`,
  `domain:`, the `+net` bash suffix), the four modes, the verdict lattice,
  the hard-deny floor, session and project-local grants, grant suggestions,
  clamped child engines, and the builtin rule lists.
- `internal/policy/shellclass`: a `mvdan.cc/sh` AST classifier — wrapper
  peeling, `sh -c` recursion, opaque detection, hard-deny patterns, and a
  command table covering coreutils, build tools, git, system administration,
  interpreters, containers, Kubernetes, IaC, databases and cloud CLIs.
- `internal/sandbox`: backend detection (container, bwrap, Landlock,
  seatbelt, none), an allowlisted environment with a hard strip for anything
  key-shaped, a bubblewrap profile that hides `$HOME` behind a tmpfs, the
  Landlock re-exec helper, and a macOS `sandbox-exec` profile generator.
- `internal/workspace`: git-toplevel roots plus `--add-dir`, symlink-safe
  `Resolve`, `.gitignore`/`.wrightignore`/`.aiignore`/`.aiexclude`, and the
  secret- and protected-path predicates.
- `internal/redact`: high-confidence secret patterns (private keys, provider
  keys, GitHub tokens, AWS, Google, Slack, Stripe, npm, PyPI, Hugging Face,
  JWTs, auth headers, URL userinfo) with a visible marker and a
  line-buffered writer.
- `internal/mcpclient`: MCP servers behind consent and a trust record. Stdio
  servers run inside the OS sandbox with the filtered environment and no
  network unless declared; HTTP servers go through the ssrfguard client. The
  first connection shows the command or URL and every tool with its
  annotations, then records the binary hash, arguments, URL and tool list;
  any change re-asks with a diff. Tools register as `mcp_<server>_<tool>`,
  are addressed by policy as `mcp:<server>:<tool>` and default to asking,
  with `readOnlyHint` carried through as advisory only.
  `wright mcp list|add|remove` and `/mcp`.
- `internal/trust`: project settings and MCP servers accepted by hash, so a
  project's `allow`, `additionalDirectories`, `passEnv` and `mcpServers` stay
  inert until you accept them; its `ask`/`deny` always apply.

**State**

- `internal/session`: `agentkit` FileStore plus `sessions/meta/<id>.meta.json`
  sidecars — list, get, touch, latest, export to Markdown, delete and purge;
  `--resume` and `--continue`.
- `internal/audit`: a SHA-256-chained JSONL log per session with redacted and
  capped arguments, `Verify`, and an end-of-run `Summary`.
- `internal/snapshot`: content-addressed pre-edit snapshots per run, with
  restore and pruning, behind `/undo`.
- `internal/config`: layered settings (embedded `defaults.json` < user <
  project < project.local < `WRIGHT_*` environment < flags) with
  append-and-dedupe rule lists and atomic 0600 saves; `internal/git` status,
  diff and tracked lookups with a 2 s timeout; `internal/theme` lipgloss v2
  palette from the gloam tokens.
- `internal/app`: the composition root that wires all of the above, and the
  `TestImportDAG` and `TestNoUnexpectedNetwork` guards over it.

**Release plumbing**

- GoReleaser (archives, checksums, `dockers_v2` image on ghcr.io, Homebrew
  cask), `Containerfile` on wolfi-base plus `Containerfile.local` for podman,
  CI / release / pages / gloam-sync workflows, Dependabot, and the gloam
  documentation site under `docs/`.

### Security

- The shell classifier called several commands that execute arbitrary code
  "safe-read", and a safe-read command is auto-allowed with no prompt:
  `awk 'BEGIN{system("…")}'` (the program text was never inspected),
  GNU `sed`'s `e` command and `s///e` flag, and `git -c` keys such as
  `diff.external`, `core.pager` and `core.sshCommand` (the key was skipped
  after checking only `core.hooksPath`). `go test -exec`, `go build
  -toolexec` and `go vet -vettool` rode the builtin `bash(go test *)` rule
  the same way, `find -exec` hid its payload's paths from the deny rules, and
  `sort -o` truncated a file while classifying as a read. All of these now
  classify as opaque, privileged or writing, so none can be auto-allowed.
- An environment-prefix assignment is invisible to an argv-prefix allow rule,
  so `GOFLAGS=-toolexec=./pwn.sh go build ./...` matched the builtin
  `bash(go build *)` rule and ran `pwn.sh` with no approval and no audit of
  the variable. The classifier now treats the build and toolchain variables
  whose value is code as opaque — `GOFLAGS`, `GOEXPERIMENT`, `GOPROXY`,
  `GOPRIVATE`, `CC`/`CXX`/`CGO_*`, `RUSTFLAGS`, `RUSTC_WRAPPER`,
  `CARGO_BUILD_RUSTFLAGS` and `CARGO_TARGET_*`, `MAKEFLAGS`, `PIP_INDEX_URL`,
  the JVM `*_OPTS` agents and the Perl/Ruby option variables — whether they
  are written as a prefix, through `env`, or exported. They are exported as
  `shellclass.InjectingEnvVars` so the sandbox's environment strip and the
  classifier cannot drift apart.
- The workspace containment checks now run *before* the allow rules. An
  argv-prefix rule such as the builtin `bash(grep *)` matches on the command
  alone, so it covered paths the command was never checked against: a
  protected path is now denied whatever rule matched, in every mode, and for
  bash an outside-the-workspace ask cannot be waived by a rule that never
  examined the path.
- A recursive reader declares the directory it was pointed at, not the files
  it opens, so `grep -r "" .` put `.env` in the model's context while
  `cat .env` was denied. A bounded scan of declared directory reads now
  forces an approval naming the credential files at stake; no rule waives
  it, and the denial says so rather than suggesting an `--allow` that would
  not work.
- Plan mode — the most restrictive mode — was the most permissive one for
  shell reads: it decided before the containment checks ran, so a read-only
  command could reach `/etc/passwd` there while default mode denied it.
- Nested `.git` and `.wright` directories are protected like the workspace
  root's. A submodule's or vendored checkout's hooks run outside the sandbox
  just the same, and were one approval away.
- Project instruction files (`AGENTS.md` from the workspace root down to cwd,
  `.wright/instructions.md`, a confirmed `CLAUDE.md`) are no longer quoted
  into the system prompt as if the system had written them. They are now
  preceded by framing that says what they are, and each block carries both
  its source and a `scope`: a `scope="project"` file arrived with the
  repository, is the user's project configuration rather than a system
  instruction, and can never widen permissions, lift a denial, change what is
  reported to the user or redirect the task. The user's own global
  `AGENTS.md` keeps its standing as `scope="user"`. Every body is scanned
  with `prompt.ScanInjection` when it is loaded: the signals ride along on
  the `Instruction`, the block carries a `warning` attribute naming them, and
  the app emits a notice naming the file — an instruction file that scans
  positive is loaded with the framing and the warning, never silently. A body
  can neither close its own block (`</instructions`) nor forge an opening tag
  (`<instructions`); both sequences are escaped.
- `redact.Writer` — the writer behind `bash`'s live output stream, which is
  what the user's screen and the transcript show — is line-buffered, so the
  only multi-line pattern (`private-key`) could never match: the model-facing
  result was redacted while the user watched the whole key scroll past. The
  writer now keeps a bounded lookbehind, holding output from a
  `-----BEGIN … PRIVATE KEY` marker until the matching `-----END`, a 64 KiB
  cap, or `Close`; an unterminated block is redacted from the marker on, and
  a block that overruns the cap is marked and its remainder dropped until the
  END marker. Ordinary output still streams line by line.
- `bash` and `web_fetch` redact before they clip. `Clip` writes the *full*
  text to the spill file under `$XDG_CACHE_HOME/wright/spill/<session>/` and
  hands the model that path, so redacting afterwards left the unredacted
  secret on disk. Nothing unredacted is written now, and the truncation note
  counts the bytes that were actually saved.
- `bash`'s `Describe` resolves relative paths against the working directory
  the command will run in, not the workspace root. The tool runs with
  `spec.Dir` set to the tracked cwd, which `cd` moves, so every declared read
  and write in the policy request — and in the approval preview the user
  reads — could name a different file from the one the command opens. It
  failed safe with a single root; a second root at another depth
  (`--add-dir`) would have made it exploitable.
- `web_fetch` refuses local, private and metadata hosts itself, before the
  request and before the approval prompt shows a URL that could never work.
  The ssrfguard client already blocked these at dial time; the tool-side
  check now covers the spellings a text glob misses — `::1`,
  `::ffff:127.0.0.1`, `0.0.0.0`, `[::]`, `2130706433`, `0x7f000001`,
  `0177.0.0.1`, `*.localhost`, `*.internal`, `metadata.google.internal`,
  link-local (169.254/16, fe80::/10), RFC1918 and carrier-grade NAT.
- `web_fetch` matches the robots.txt product token exactly. It used a prefix
  match, so a site's `User-agent: wrightbot` group captured `wright` and
  applied another crawler's rules to us.
- Sandboxed commands run as `bash -c`, not `bash -lc`. A login shell sources
  `/etc/profile`, `/etc/profile.d/*` and — on the `none` backend, where
  `$HOME` is the user's own — `~/.bash_profile`, any of which can put back
  what the sandbox's filtered environment deliberately left out. The
  environment handed to the command is explicit and already carries `PATH`.
- The sandbox keeps the paths that decide what happens *outside* it
  read-only. Every workspace root was bound read-write as a whole, so any
  code-execution primitive inside `bash` could write
  `.git/hooks/pre-commit` — which then runs unsandboxed on the user's next
  commit — or `.wright/settings.local.json`, which decides wright's own
  permissions next session. bwrap re-binds `.git/hooks`, `.git/config`,
  `.git/config.worktree`, a `.git` *file* (worktree or submodule pointer)
  and `.wright` read-only after the read-write bind of the root, and masks
  wright's own config and state directories with a tmpfs, so
  `sandbox.extraReadWrite` naming a parent cannot re-expose them. Landlock
  rules are additive — the kernel unions every rule matching an ancestor, so
  a subtree can never be subtracted from a read-write rule — so the roots
  are granted entry by entry instead, which also means a shell command under
  Landlock cannot create new entries directly in the workspace root or in
  `.git`; the backend says so rather than leaving it to be discovered.
  seatbelt denies the same paths after the allows. The rest of `.git` stays
  writable: `git add` and `git commit` have to keep working inside the
  sandbox, and the hook is what escapes it.
- Landlock's "no network" is a network namespace, not just `RestrictNet`.
  Landlock governs TCP bind and connect only, so `Spec.Network=false` left
  UDP, ICMP and raw sockets fully usable — a complete exfiltration channel
  behind a status bar that said the network was off. The helper is now
  re-executed in an empty network namespace (unprivileged user namespace,
  identity uid/gid map, capabilities dropped by `execve`), the same
  confinement bwrap gets from `--unshare-all`. Where the kernel forbids
  unprivileged user namespaces the backend reports that it restricts TCP
  only instead of claiming more, and `wright doctor` prints what actually
  confines the network rather than quoting the ABI version.
- `.wright/settings.local.json` is trust-gated with `.wright/settings.json`.
  `checkTrust` returned "trusted" when `settings.json` was absent, and the
  project-local layer was merged unconditionally on top of the gated one, so
  a repository shipping only `settings.local.json` chose the permission
  mode, added blanket allow rules, extra directories, sandbox mounts, the
  network switch and `redaction: false` — while stderr printed a note about
  `settings.json` "not being trusted" that read as an assurance and was not
  one. The two files are now hashed and accepted as a unit, both go through
  the tightening-only filter when untrusted, and the note names everything
  that was dropped. An untrusted layer may still tighten: `ask` and `deny`
  rules and `redaction: true` always apply.
- The audit hash chain is anchored outside the log. `Verify` recomputed
  `prev` from the file it was checking, so it only ever proved the log was
  self-consistent: whoever could write the file could cut the tail off (the
  chain runs backwards, so a shorter log verifies perfectly) or edit a line
  and recompute every `prev` after it. Each written line now records the new
  head — sequence number and line hash — in a 0600 file under the user's
  config directory, and `wright audit verify` checks the log against it,
  reporting truncation and a recomputed chain as different things. A log
  with no recorded head reports that rather than "intact", because deleting
  the anchor is the first step of the attack.
- The sandbox environment no longer passes variables that inject code into
  an allowed command. `GO*` was globbed and `NODE_OPTIONS` was listed:
  `GOFLAGS` carries `-toolexec` and `-ldflags`, so an allowed `go build`
  could run an arbitrary binary with nothing in the script the policy engine
  saw; `NODE_OPTIONS` carries `--require`; `GOPROXY` routinely carries
  `https://user:pass@host` credentials; `GOPRIVATE`/`GONOSUMDB`/`GOSUMDB`
  turn module checksum verification off. The Go and Node variables are named
  one by one now (locations and target selectors), and the injecting ones —
  with `LD_PRELOAD`, `BASH_ENV`, `PERL5OPT` and their relatives — are in the
  hard-strip set, so `sandbox.passEnv` from a trusted settings file cannot
  reinstate them either.
- The landlock backend protects `.git/hooks`, `.git/config` and `.wright`
  with read-only bind mounts instead of by withholding write on the
  workspace root. Landlock rules are additive, so the first fix kept those
  paths read-only the only way Landlock can — by granting the root entry by
  entry — and that also stopped a shell command creating *any* new file or
  directory in the repository root. The shipped container image selects
  landlock (`WRIGHT_SANDBOX=landlock`, no bwrap), so `podman run
  ghcr.io/richardwooding/wright` got exactly that, and an agent that cannot
  create a file in the repository root is not an agent. The helper now runs
  in a mount namespace and bind-mounts those paths read-only over
  themselves, which is what bwrap does, and the root is granted read-write
  as normal: `touch NEWFILE`, `mkdir sub` and `git commit` all work while
  the hook stays unwritable. Where a filesystem refuses the remount — a
  volume a container runtime bind-mounted in is locked by the user namespace
  that owns it — commands keep working and the backend says plainly that
  those paths rest on the permission layer alone.
- Protected paths that do not exist yet are created before the sandbox is
  built. Binding only what existed left the protection defeatable by
  `mkdir .wright && echo … > .wright/settings.local.json`, which landed on
  the host from inside the sandbox. `sandbox.EnsureProtected` creates
  `.wright`, and `.git/hooks` and `.git/config` when `.git` is a directory,
  in the workspace roots only — never a `.git` of wright's own invention,
  which would make git treat a plain directory as a repository.
- Nothing may replace a sandboxed command's process attributes. The bash
  tool set its process group by assigning a fresh `SysProcAttr`, discarding
  the clone flags that put the command in its own network and mount
  namespaces — silently, with the status bar still reporting "landlock". It
  adds to them now, and the helper compares its namespaces against the
  backend's and refuses to run the payload if it is still in the parent's,
  so the same mistake cannot be made quietly again.
- A malformed `.wright/settings.json` or `.wright/settings.local.json` no
  longer stops wright from starting in that directory. A repository could
  ship one — or a payload with a single write could leave an empty file
  behind — and every later run failed at startup with a bare `EOF` from the
  JSON decoder. Decode errors now name the file and the line ("invalid JSON
  at line 3", "the file is empty"), and a project layer that will not parse
  is reported and dropped, matching how an untrusted project layer is
  already dropped. The user's own `config.json` stays fatal: it is theirs to
  fix.
