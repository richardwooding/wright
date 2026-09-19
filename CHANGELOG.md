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
