# wright

[![Go Reference](https://pkg.go.dev/badge/github.com/richardwooding/wright.svg)](https://pkg.go.dev/github.com/richardwooding/wright)
[![ci](https://github.com/richardwooding/wright/actions/workflows/ci.yml/badge.svg)](https://github.com/richardwooding/wright/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

**Website:** [richardwooding.github.io/wright](https://richardwooding.github.io/wright/)

A coding agent for your terminal. `wright` runs an agent loop against your
repository with the tools a coding task actually needs — read, write, edit,
glob, grep, list, shell, fetch — in a Bubble Tea TUI or headless behind `-p`.
It is built on [`agentkit`](https://github.com/richardwooding/agentkit) +
[`llmkit`](https://github.com/richardwooding/llmkit), so any model llmkit can
open works by name: Anthropic, OpenAI, Vertex, xAI, DeepSeek, OpenRouter,
Groq, or a model already running in your local Ollama. Point it at a repo with
`wright`, or script it with `wright -p "…"`.

What makes it different is what sits between the model and your machine.
Every tool call goes through an approval-first permission engine with a
hard-deny floor that no mode — not even bypass — can lift. Every shell command
runs inside an OS sandbox with the network off unless you grant it, and shell
scripts are classified from a real bash AST, so nothing opaque can ever match
a rule you wrote. Your provider keys never enter that sandbox and
secret-looking strings are redacted out of tool output before the model sees
them. There is no telemetry and no update check. The system prompt tells the
model that denials are final and that it may only report outcomes it actually
observed, and every call, verdict, file change and command lands in a
SHA-256-chained audit log you can verify afterwards. Project instructions come
from `AGENTS.md`, the cross-tool standard.

## Install

**Homebrew** (macOS and Linuxbrew):

```sh
brew install --cask richardwooding/tap/wright
```

**Go:**

```sh
go install github.com/richardwooding/wright/cmd/wright@latest
```

**Container** (the image carries bash, git, coreutils and ripgrep; inside it
the container itself is the sandbox boundary):

```sh
podman run --rm -it --userns=keep-id -v "$PWD:/workspace:Z" -e ANTHROPIC_API_KEY ghcr.io/richardwooding/wright
```

`--userns=keep-id` maps your user into the container so the agent can write to
the mounted workspace; without it rootless Podman maps the image's user to a
different host uid and every write fails with "permission denied". It also lets
the sandbox apply its read-only mounts over `.git/hooks` and `.wright` — wright
says so on stderr when it cannot.


Or download a prebuilt binary for macOS or Linux from the
[releases page](https://github.com/richardwooding/wright/releases). It is a
single static binary — no cgo, no runtime dependencies. `git`, `rg` and
`bwrap` are used when present; `wright doctor` shows what was found.

## Quick start

```sh
export ANTHROPIC_API_KEY=…     # or OPENAI_API_KEY, XAI_API_KEY, … or just run Ollama
wright                         # interactive session in the current repo
wright "add tests for the parser"
wright -p "explain the build"  # headless: one prompt, one answer
```

```
$ wright -p "say hello" -m fake:latest
Hello from the fake model. How can I help?
```

| flag | meaning |
| --- | --- |
| `-p, --print` · `--output text\|json\|stream-json` | headless mode and its report format |
| `-m, --model` | model name (`WRIGHT_MODEL`); otherwise detected from the credentials present |
| `--mode` | `default`, `plan`, `auto-edit` (`WRIGHT_MODE`); bypass only via `--bypass-permissions` |
| `--allow` / `--deny` | permission rules for this run (repeatable) |
| `--sandbox` · `--allow-network` | backend `auto\|bwrap\|landlock\|seatbelt\|none` (`WRIGHT_SANDBOX`); let sandboxed commands reach the network |
| `-r, --resume` / `-c, --continue` | resume a session by ID, or the latest for this workspace |
| `--cwd` · `--add-dir` | working directory; extra directories the agent may access (repeatable) |
| `--max-steps` · `--reasoning` | step budget per run; reasoning effort hint (`low`, `medium`, `high`) |
| `--strict-injection` | treat prompt-injection signals in web/MCP content as needing approval |
| `--plain` · `-v` · `-V` | no alternate screen (`WRIGHT_PLAIN`); verbose diagnostics on stderr; version |

`wright doctor` is the first thing to run on a new machine: it reports which
sandbox backends are available, the Landlock ABI the kernel gives you, whether
`git` and `rg` are on PATH, and which provider credentials are set — by
**name** only, never by value.

```
$ wright doctor
wright dev (none, unknown)

Sandbox
  ! container    not running inside a container
  ✓ bwrap        bubblewrap 0.11.0
  ✓ landlock     available
  ! seatbelt     not supported on this platform
  · landlock ABI v8 (network restriction needs v4+)
  ✓ selected     bwrap

Tools
  ✓ git          git version 2.55.0
  ✓ rg           ripgrep 15.2.0
  ✓ bwrap        bubblewrap 0.11.0

Credentials (names only)
  · ANTHROPIC_API_KEY absent
  …
```

## How permissions work

Every tool call is turned into a request (tool, resolved paths, writes, shell
analysis, URL) and evaluated as a lattice, with no specificity scoring:

```
hard-deny set → deny rules → ask rules → allow rules / session grants → mode table
```

### Modes

`--mode default|plan|auto-edit`, or `shift+tab` in the TUI. Bypass is not a
settings value: only `--bypass-permissions` can reach it.

| | reads in workspace | edits in workspace | bash: read-only | bash: mutating | bash: network | bash: opaque | privilege / `sudo` | web_fetch | outside the workspace | MCP |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| **default** | allow | ask | allow | ask | ask | ask | deny | ask | ask; secret/protected deny | ask |
| **plan** | allow | deny | allow | deny | deny | deny | deny | ask | reads ask, writes deny | read-only-annotated: ask, else deny |
| **auto-edit** | allow | allow (not ignored/sensitive) | allow | ask | ask | ask | deny | ask | ask; secret/protected deny | ask |
| **bypass** | allow | allow | allow | allow | allow (sandbox network still off unless `--allow-network`) | allow | **deny** | allow | allow except hard-deny | allow |

A command is "opaque" when the analyser cannot see through it — `eval`,
`source`, `$(…)` in a command position, `sh -c "$X"` — and an opaque script can
never match an allow rule, in any mode. Privilege commands are refused in
every mode, bypass included. "Outside the workspace" means outside the git
toplevel of your cwd plus anything you added with `--add-dir`, with every path
resolved through `EvalSymlinks` first, so a symlink out of the tree is seen
for what it is. Plan mode still *asks* for a read outside the workspace rather
than refusing it outright; everything that would change state there is denied.

### Rules

Rules live under `"permissions"` in `~/.config/wright/config.json`,
`.wright/settings.json` (shared) and `.wright/settings.local.json` (yours,
gitignored), and on the command line with `--allow` / `--deny` (repeatable).

```
rule    := tool [ "(" spec ")" ] [ "+net" ]
tool    := read_file | write_file | edit_file | glob | grep | list_dir | bash
         | multi_edit | web_fetch | web_search | todo_write | ask_user
         | explore | <agent> | skill | skill_file
         | "mcp:" server [ ":" toolglob ] | "*"
spec    := pathglob                    doublestar; relative = workspace-relative; "~/" and "$WORKSPACE/" expand
         | argv-prefix [ "*" ]         bash: matched per simple command after the AST split
         | "re:" RE2                   bash, advanced
         | "domain:" host | "*.host"   web_fetch
```

```jsonc
{
  "permissions": {
    "allow": ["bash(go test *)", "bash(curl -sS https://pkg.go.dev/*) +net",
              "edit_file($WORKSPACE/internal/**)", "web_fetch(domain:*.pkg.go.dev)"],
    "ask":   ["bash(git push *)"],
    "deny":  ["*(**/*.tfstate)", "bash(docker *)"]
  }
}
```

`+net` is a bash-only suffix: it allows the command *and* grants the sandbox
network for it. A script is allowed by rules only when **every** command in it
matches an allow rule and nothing in it is opaque. `*` is refused in allow
lists. An "always allow" offer in the approval prompt is never made for
destructive or opaque commands and never widens beyond two argv words.

The builtin defaults (`internal/policy/builtin.go`, mirrored byte-for-byte in
the embedded `internal/config/defaults.json`) are:

- **allow** — `read_file`, `glob`, `grep`, `list_dir` under `$WORKSPACE/**`;
  `bash(git status|diff|log|show|blame|branch --list|remote -v|rev-parse|stash list|worktree list *)`;
  `bash(go build|test|vet|fmt *)`; `bash(rg|grep|find|ls|cat|head|tail|wc *)`.
- **ask** — `write_file($WORKSPACE/**)`, `edit_file($WORKSPACE/**)`,
  `multi_edit($WORKSPACE/**)`,
  `bash(git push *)`, `web_fetch`, `web_search`, `mcp:*`.
- **deny** — `.env`, `*.pem`, `*.key`, `*.p12`, `*.pfx`, `*.jks`, `*.kdbx`,
  `credentials*`, `service-account*.json`; `~/.ssh`, `~/.aws`, `~/.gnupg`,
  `~/.kube`, `~/.config/gh`, `~/.config/wright`, `~/.docker/config.json`,
  `~/.netrc`, `~/.npmrc`, `~/.pypirc`; `$WORKSPACE/.wright/**` and
  `$WORKSPACE/.git/**`; `bash(sudo|su|doas|pkexec|crontab|systemctl|launchctl *)`;
  `web_fetch(domain:169.254.169.254|localhost|127.*)`.

Builtin *ask* rules rank below allow rules (they are the mode table written as
rules, so your `allow` for `edit_file` can take effect); explicit ask rules
from a user, project or flag rank above them, so tightening always wins.

### The hard-deny floor

Checked before rules and before the mode table, in every mode including
bypass, and counted: three hard denials cancel the run.

- reading a secret file; writing to a protected path (`~/.ssh`, `.git/**`,
  `.wright/**`, `~/.config/wright/**`, shell rc files, `/etc/**`);
- `sudo`, `su`, `doas`, `pkexec`;
- `curl … | sh`, `wget … | bash`, decode-to-shell (`base64 -d | sh`);
- `rm -rf` resolving to `/`, `$HOME` or a workspace root;
- force-pushing or deleting a protected branch (`main`, `master`,
  `release/*`, configurable);
- `git config core.hooksPath`, `PATH=` overrides via `env -i`;
- anything under a `.wrightignore` path, which is invisible to every tool.

Hard denials live in the shell classifier and the path checks, never as
builtin deny rules, because a rule can be shadowed by a mode and this floor
cannot.

### Headless never auto-approves

In `-p` mode there is nobody to ask, so every `Ask` verdict becomes a denial
whose message names the rule or mode that would have permitted it, and the run
exits `3`:

```
$ wright -p "run curl please" --output stream-json
…"result":{"text":"not approved: bash requires interactive approval in headless mode (re-run with --allow 'bash(curl -sS *) +net' --allow 'bash(head -c *)', or use --mode auto-edit for edits)","is_error":true}…
…"exit_code":3}

$ wright -p "run curl please" --allow 'bash(curl *) +net' --allow 'bash(head *)'
(runs; the output comes back fenced as <untrusted source="bash">)
```

There is no timeout-approves path, and nothing is ever approved because the
model asked twice.

### Bypass friction

`--bypass-permissions` is a flag only — never a settings key, never an
environment variable, never inherited by a sub-agent. It is refused when
running as root outside a container, and refused when no sandbox backend is
available unless you also pass `--allow-unsandboxed-bypass`. In the TUI it
needs a typed confirmation, the mode segment turns hot red, and the hard-deny
floor still applies.

## MCP servers

```sh
wright mcp add docs --command docs-mcp-server --arg=--stdio --env DOCS_TOKEN
wright mcp add issues --type http --url https://example.test/mcp
wright mcp list
wright mcp remove docs
```

`add` writes the server into `.wright/settings.json`; `--env` takes variable
**names**, whose values are read from wright's own environment when the server
starts, so a committed settings file never carries a secret. A stdio server
runs inside the OS sandbox with the filtered environment, the workspace as its
working directory and no network unless the entry says `"network": true`.

On the first connection wright lists the server's command (or URL) and every
tool it offers, with the annotations the server claims, and records what it
showed — the server binary's hash, its arguments, the URL and the tool list —
in `~/.config/wright/trust.json` once accepted. Any change to those re-asks and
says what changed. Tools appear to the model as `mcp_<server>_<tool>` and to
the permission rules as `mcp:<server>:<tool>`, which the builtin `mcp:*` ask
rule covers; `readOnlyHint` and friends are the server's claims, so they are
shown to you and can only *tighten* what plan mode allows, never approve
anything. `/mcp` shows what the current session connected.

The interactive consent prompt is not built yet: until it lands, an
unaccepted server is reported in full and declined, and you accept it by
adding `"trusted": true` to its entry in a settings file that is itself
trusted.

## Skills

[Agent Skills](https://agentskills.io) are Markdown instructions (plus files)
that the model loads on demand. wright looks in, lowest precedence first:

```
~/.config/wright/skills   <ws>/.agents/skills   <ws>/.wright/skills
skills.extraDirs          <ws>/.claude/skills   (only with "loadClaudeSkills": true)
```

The catalog of names and descriptions goes in the system prompt; the `skill`
tool returns a skill's full text and `skill_file` reads the files it ships.
Neither can change anything: a script a skill ships runs only if the model
calls `bash`, where the permission engine and the sandbox apply as usual.
`wright skills` and `/skills` list what was found, and a malformed `SKILL.md`
is reported without costing you the others.

## Sub-agents

`explore` is built in: a read-only research agent (read tools only, the fast
model, a short budget) that answers a question about the codebase and reports
back, keeping a long search out of the main transcript. Its work appears
indented in the transcript, one level deeper.

Custom agents are Markdown files in `.wright/agents/` or
`~/.config/wright/agents/`:

```markdown
---
name: reviewer
description: Review a diff for correctness and house style
tools: read_file, grep, glob, list_dir
model: anthropic/claude-haiku-4-5
read-only: true
---

Review the change. Report findings with file paths and line ranges…
```

`tools` limits the agent to those tools; without it a read-only agent gets the
read tools and a `read-only: false` agent gets everything except `bash`. A
sub-agent is never a way around permissions: every call it makes is evaluated
again, one level deeper, by a child policy engine that clamps bypass back to
the default mode and cannot grant anything. `/agents` lists them.

## Sandbox

Every `bash` command runs through a backend, detected in this order:
**container** (`/.dockerenv`, `/run/.containerenv`, `WRIGHT_CONTAINER=1` — the
container is the boundary, policy still applies) → **bwrap** → **landlock** →
**seatbelt** (macOS `sandbox-exec`) → **none**. Force one with
`--sandbox bwrap|landlock|seatbelt|none` or `WRIGHT_SANDBOX`.

- **bubblewrap**: `--unshare-all`, a read-only `/`, a tmpfs over `$HOME`, then
  the workspace roots and declared build caches bound back read-write.
- **Landlock**: wright re-executes itself as a hidden `__sandbox` helper which
  applies a best-effort ruleset (system directories read-only by name — never
  `RODirs("/")`) and then `exec`s the command. Network restriction needs ABI
  v4+; `doctor` reports the ABI actually achieved.
- **none**: the status bar says `sandbox off` in hot red, headless warns once,
  and bypass mode is refused.

The network is off inside the sandbox unless a `+net` rule matched, you
approved a call the classifier says needs it, or you passed `--allow-network`.
**Approving a call grants what that call needs**: when the classifier says a
command cannot work without the network, allowing it runs it with the
network, and the prompt says so before you answer — an approval that leaves
the command unable to work is a trap, not a safeguard. The model can never
grant itself network: asking for it is what turns an allow into an ask, and a
*saved* rule still needs an explicit `+net`.

Installing software is the same idea one step further. A package manager
writes outside the workspace — the Homebrew prefix and cache, the npm global
prefix, `~/.cargo`, `$GOBIN`, `~/.local/bin`, the gem home — which the sandbox
mounts read-only. When the classifier recognises an install (`brew install`,
`go install`, `npm install -g`, `pipx install`, `cargo install`, …), the
approval prompt **names those directories** and allowing the call mounts them
read-write **for that one call**. Nothing else widens them: not a saved allow
rule, not bypass mode, and never the base sandbox.

The environment a sandboxed command sees is an **allowlist** (`PATH`, `HOME`,
`LANG`, `TERM`, `XDG_*`, `GO*`, `CARGO_HOME`, `NODE_OPTIONS`, `PYTHONPATH`,
`VIRTUAL_ENV`, `JAVA_HOME`, `CI`, `NO_COLOR`, plus names you passed through in
trusted settings), and anything matching `*_KEY`, `*_TOKEN`, `*_SECRET`,
`*_PASSWORD`, `AWS_*`, `GITHUB_TOKEN`, `LLMKIT_*`, `DATABASE_URL` is stripped
even if it was passed through explicitly.

## Sessions and audit

Sessions are append-only JSONL transcripts plus a metadata sidecar under
`$XDG_DATA_HOME/wright/projects/<hash>/`, keyed by workspace.

```
$ wright sessions list
ID                    UPDATED           TURNS  MODEL        COST  TITLE
20260919-204014-788e  2026-09-19 20:40  1      fake:latest  —     please list the files here
```

```sh
wright --continue            # resume the most recent session for this workspace
wright --resume 20260919-204014-788e
wright sessions show <id>
wright sessions export <id> -o transcript.md
wright sessions delete <id>          # transcript, metadata, snapshots and audit log
wright sessions purge --older-than 720h
```

Every call, verdict, file change, command and model call is appended to a
SHA-256-chained JSONL log, one per session, mode 0600, alongside the
transcript. Arguments are redacted and capped at 4 KiB with the full value's
hash recorded; environment variables and request bodies are never logged.

```
$ wright audit verify
ok: 8 event(s), chain intact (…/audit/20260919-204014-788e.jsonl)
```

```sh
wright audit show                 # the latest session, as summary lines
wright audit show <id> --kind decision
wright audit show <id> --json     # raw events
```

`/undo` in the TUI reverts the file changes of the last run from
content-addressed pre-edit snapshots, and says plainly that shell side effects
are not reverted.

## Headless / CI

```sh
wright -p "explain the build"                                   # final text on stdout
wright -p "which tests cover the parser?" --output json         # one result object
git diff | wright -p "review this change" --output stream-json  # stdin is fenced as <stdin>
wright -p "rename Foo to Bar in internal/x" --allow 'edit_file(**)'
wright -p "run the tests and fix what fails" --allow 'bash(go test *)' --mode auto-edit -v
wright -p "…" --max-steps 20
```

| code | meaning |
| --- | --- |
| `0` | ok |
| `1` | provider or tool error, or the run stopped with an error |
| `2` | usage: no prompt, unknown `--output` format |
| `3` | approval required — a call was denied because nobody could answer; the message names the `--allow` rule or mode that would permit it |
| `4` | budget exhausted (`--max-steps`, max tokens, deadline) |
| `130` | interrupted (SIGINT/SIGTERM); the run is cancelled and the audit `run_end` is still written |

`--output stream-json` emits one JSON object per line. `type` is the event
kind in snake_case (`run_started`, `step`, `text`, `reasoning`, `tool_call`,
`tool_progress`, `tool_result`, `usage`, `approval_request`,
`approval_decided`, `question`, `todos`, `queued`, `notice`, `error`), and
every other field appears only when it applies: `time`, `run_id`, `depth`,
`step`, `text`, `tool {name,id,args}`, `result {text,is_error}`,
`usage {input_tokens,output_tokens,cached_input_tokens,…}`, `cost_usd`,
`context_pct`, `approval`, `decision`, `todos`, `queued`, `error`. The last
line is always `type: "result"` and carries `output`, `stop_reason`, `steps`,
`tool_calls`, `usage`, `cost_usd`, `duration_ms`, `session_id` and
`exit_code` (always present, even when `0`).

```
$ wright -p "please list the files here" --output stream-json
{"type":"run_started","time":"…","run_id":"f8c90ea1a4f6be77"}
{"type":"step","time":"…","run_id":"f8c90ea1a4f6be77","step":1}
{"type":"usage","time":"…","run_id":"f8c90ea1a4f6be77","step":1,"usage":{"input_tokens":40,"output_tokens":8},"context_pct":0.0003125}
{"type":"tool_call","time":"…","run_id":"f8c90ea1a4f6be77","step":1,"tool":{"name":"list_dir","id":"call_1","args":{"path":"."}}}
{"type":"tool_result","time":"…","run_id":"f8c90ea1a4f6be77","step":1,"tool":{"name":"list_dir","id":"call_1","args":{"path":"."}},"result":{"text":"<untrusted source=\"list_dir\">\n./\n  main.go\n</untrusted>"}}
{"type":"step","time":"…","run_id":"f8c90ea1a4f6be77","step":2}
{"type":"text","time":"…","run_id":"f8c90ea1a4f6be77","step":2,"text":"The workspace contains the files listed above. Done."}
{"type":"usage","time":"…","run_id":"f8c90ea1a4f6be77","step":2,"usage":{"input_tokens":42,"output_tokens":12},"context_pct":0.000328125}
{"type":"result","usage":{"input_tokens":82,"output_tokens":20},"output":"The workspace contains the files listed above. Done.","stop_reason":"completed","steps":2,"tool_calls":1,"duration_ms":1,"session_id":"20260919-204014-788e","exit_code":0}
```

Every tool result reaches the model fenced as
`<untrusted source="…">…</untrusted>`: tool, file, web, MCP and sub-agent
content is data, not instructions, and the prompt says so.

## Configuration

Settings are merged in layers, each overriding the one before; rule lists
accumulate (append + dedupe) rather than replacing.

```
embedded defaults.json  <  ~/.config/wright/config.json
                        <  .wright/settings.json          (shared, trust-gated)
                        <  .wright/settings.local.json     (yours, gitignored)
                        <  WRIGHT_* environment
                        <  command-line flags
```

**Project settings are inert until you trust them.** A project's `allow`
rules, `additionalDirectories`, `sandbox.passEnv`, `search.provider` and
`mcpServers` do not
apply — and a warning says so — until `/trust` (or `wright init` having
written the file) records the file's hash in `~/.config/wright/trust.json`;
any later edit re-prompts with a diff. A project's `ask` and `deny` rules
always apply, because tightening is free. `wright config show` prints the
gated view; `wright config paths` prints where each layer lives.

```jsonc
{
  "model":       { "default": "", "fast": "", "reasoning": "", "maxOutputTokens": 0, "contextWindow": 0 },
  "permissions": { "mode": "default", "allow": [], "ask": [], "deny": [], "additionalDirectories": [] },
  "sandbox":     { "backend": "auto", "allowNetwork": false,
                   "extraReadOnly": [], "extraReadWrite": [], "passEnv": [] },
  "mcpServers":  {},
  "skills":      { "extraDirs": [], "loadClaudeSkills": false },
  "search":      { "provider": "" },
  "git":         { "attribution": true,
                   "trailer": "Co-Authored-By: wright <wright@richardwooding.github.io>",
                   "protectedBranches": ["main", "master", "release/*"] },
  "redaction":   true,
  "instructions": { "files": ["AGENTS.md", ".wright/instructions.md"], "fallback": "ask" },
  "updates":     { "check": false }
}
```

Environment: `WRIGHT_MODEL`, `WRIGHT_MODE`, `WRIGHT_SANDBOX`, `WRIGHT_PLAIN`,
`WRIGHT_CONFIG_DIR`, `WRIGHT_DATA_DIR`, `WRIGHT_CONTAINER`, plus `NO_COLOR`
and `XDG_CONFIG_HOME` / `XDG_DATA_HOME`.

### Web search

`web_search` is **off unless you name a provider**, so wright never sends a
query anywhere by default. Set `search.provider` and put the key in the
matching environment variable:

| provider | key | endpoint |
| --- | --- | --- |
| `brave` | `BRAVE_API_KEY` | `api.search.brave.com` |

```json
{ "search": { "provider": "brave" } }
```

With no provider, or with a provider whose key is missing, the tool is not
registered at all — the model is not told it exists, and a warning on stderr
says why. Searches go through the same SSRF-guarded client as `web_fetch`,
the key is never echoed in an error, and `web_search` asks for approval in
default mode like any other network tool. A provider named by an *untrusted*
project's settings is ignored, because the query text is disclosed to whoever
the provider is.

### Project instructions

wright reads **`AGENTS.md`** — the cross-tool standard — from
`~/.config/wright/AGENTS.md` and from every directory between the workspace
root and your cwd, plus `.wright/instructions.md` for wright-specific text.
They go into the cached, stable half of the system prompt, wrapped in
`<instructions source="…">` so the model knows where each came from.

If a repository has only a `CLAUDE.md`, wright does **not** silently adopt it:

```
wright: CLAUDE.md found but no AGENTS.md: it is not loaded; run /init (or `wright init`)
to create AGENTS.md, or set instructions.fallback to always/never
```

Set `instructions.fallback` to `always` or `never` to settle it, or run
`wright init`, which writes an `AGENTS.md` and a `.wright/settings.json`
skeleton — and trusts the settings file, because you wrote it.

### Redaction

Secret-looking strings in tool output are replaced with
`[redacted: github…3f9a]` before the model sees them: private-key blocks,
`sk-ant-`/`sk-`, `gh[pousr]_`, `github_pat_`, AWS keys, Google API keys,
service-account `private_key_id`, Slack, Stripe, `npm_`, `pypi-`, `hf_`, JWTs,
`Authorization: Bearer …` and URL userinfo. Your own typed input is never
rewritten. `/redaction off` turns it off for the session.

## Keys and slash commands

| key | |
| --- | --- |
| `enter` | send (queued while a run is active, then steered in) |
| `shift+enter` / `alt+enter` / `ctrl+j` | newline |
| `esc` | cancel the run · close an overlay (which means deny) |
| `ctrl+c` `ctrl+c` | quit (twice within 1.5 s) |
| `ctrl+d` | quit when the box is empty |
| `ctrl+o` | expand / collapse all tool cards |
| `ctrl+t` | todos |
| `shift+tab` | cycle mode default → auto-edit → plan |
| `pgup` / `pgdn` | scroll the transcript a page |
| `shift+up` / `shift+down` | scroll the transcript a line |
| `alt+m` (or `/mouse`) | wheel scrolling — off by default, so your terminal can select and copy text |
| `ctrl+u` | clear the box · `ctrl+l` redraw |
| `@` / `/` | file completion · command completion |

`/help` `/clear` `/compact` `/cost` `/diff` `/mode` `/model` `/sessions`
`/resume` `/export` `/undo` `/init` `/mcp` `/skills` `/todos` `/audit`
`/redaction` `/reasoning` `/trust` `/mouse` `/plain` `/quit`

The status bar carries the whole state of the run, and the summary is printed
after the alt screen is gone:

```
wright · fake:latest · default · ctx 0% · 0 tok · $0.00 · bwrap ⊘net · master ?1 · sess 20260919-…

steps 1 · tool calls 0 · 39 tok · — · 0s · completed
session 20260919-204145-33e6 — resume with wright --resume 20260919-204145-33e6
60 in / 18 out tokens
```

`sandbox off` and `⛔ bypass` are hot red and bold — the two states you must
not miss. Every status is glyph **and** word, never colour alone, and the
approval prompt focuses *deny* for destructive requests. `--plain` (implied by
`NO_COLOR`, `TERM=dumb` or a non-TTY) skips the alternate screen entirely and
reads answers as numbered prompts from stdin.

## Models

`-m/--model` (or `WRIGHT_MODEL`) takes any name llmkit can route. With no
name, wright picks in this order: `--model` flag → `WRIGHT_MODEL` →
`model.default` in settings → the first provider whose credentials are present
(**Anthropic → OpenAI → Vertex → xAI → DeepSeek → OpenRouter → Groq**), each
contributing its best coding model from the llmkit catalog → a **loopback**
Ollama probe of `/api/tags`, preferring coder-tuned tags. A bare name that no
provider claims is an error, not a silent fall through to localhost.

```
$ wright models
   MODEL        PROVIDER  CONTEXT  NAME
*  fake:latest  ollama    128k

* default (detected)
```

Cost and context come from the catalog. A model the catalog does not know
still works — it is priced as `—` rather than guessed at, and the context
meter falls back to a 128k assumption.

## Privacy

What leaves your machine:

- **API calls to the model provider you chose** — or nothing at all if that
  provider is a local Ollama.
- **`web_fetch` requests you approved**, through an SSRF guard
  ([`ssrfguard`](https://github.com/richardwooding/ssrfguard)) that
  re-validates every redirect, with a rate limit, a 1 MiB cap, robots.txt and
  an honest user agent. Link-local and loopback addresses are denied by
  default.
- **MCP servers you configured and trusted**, when you configure any.
- **One loopback probe of `127.0.0.1:11434`** to see whether Ollama is
  running. Non-loopback hosts are refused unless you set `OLLAMA_HOST`
  yourself.

That is the whole list, and a test in `internal/app` (`TestNoUnexpectedNetwork`)
enforces it at the import level. There is no telemetry, no crash reporting, no
analytics and no update check — `updates.check` defaults to `false` and
nothing checks it today. Sessions, snapshots and audit logs are files on your
disk; nothing is uploaded. `wright doctor` prints credential *names* only,
never values, and the audit log never records environment variables or request
bodies.

## Status

wright is pre-1.0. v0.1.3 is the current release. Working end to end
today: the TUI,
headless mode and all three output formats, the permission engine and shell
classifier, the sandbox backends, the tool set (`read_file`, `write_file`,
`edit_file`, `multi_edit`, `glob`, `grep`, `list_dir`, `bash`, `web_fetch`,
`web_search`, `todo_write`, `ask_user`), skills discovery, MCP servers behind consent, the `explore`
sub-agent and custom agents, sessions and resume, snapshots and `/undo`,
redaction, the audit log, model detection and cost, trust-gated project
settings, and the release plumbing.

Landing next: the interactive MCP consent prompt (until then an unaccepted
server is declined with a full report of what it asked for) and background
`bash` jobs. A macOS seatbelt profile
ships but macOS is treated conservatively until it has more mileage.
**Not planned:** an LSP client — wright uses your repository's own tools
instead.

## Built on

- [`agentkit`](https://github.com/richardwooding/agentkit) — the agent loop,
  tools, middleware, approvals, sessions and compaction.
- [`llmkit`](https://github.com/richardwooding/llmkit) — multi-provider LLM
  client, streaming, and the model catalog behind cost and context.
- [`ssrfguard`](https://github.com/richardwooding/ssrfguard) — the HTTP client
  behind `web_fetch` and `web_search`.
- [Charm](https://charm.land) v2 — Bubble Tea, Lip Gloss, Bubbles, Glamour.
- [`mvdan.cc/sh`](https://github.com/mvdan/sh) — the bash parser the shell
  classifier is built on.

## License

[MIT](LICENSE)
