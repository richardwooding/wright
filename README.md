# wright

[![Go Reference](https://pkg.go.dev/badge/github.com/richardwooding/wright.svg)](https://pkg.go.dev/github.com/richardwooding/wright)
[![ci](https://github.com/richardwooding/wright/actions/workflows/ci.yml/badge.svg)](https://github.com/richardwooding/wright/actions/workflows/ci.yml)

**Website:** [richardwooding.github.io/wright](https://richardwooding.github.io/wright/)

> **Under construction.** The permission engine, shell classifier, sandbox,
> redaction and audit packages are in place and tested; the interactive
> agent lands next. `wright doctor` works today.

A coding agent for your terminal that **asks first**. `wright` runs an LLM
agent (Anthropic, OpenAI, Vertex, xAI, DeepSeek, OpenRouter, Groq or a local
Ollama) against your repository with the tools it needs — read, edit, search,
shell — and puts a permission model, an OS sandbox and an audit trail between
the model and your machine. It is built on
[`agentkit`](https://github.com/richardwooding/agentkit) +
[`llmkit`](https://github.com/richardwooding/llmkit) and the
[Charm](https://charm.land) v2 stack.

## The safety model

| guarantee | how |
| --- | --- |
| **Approval-first** | every tool call goes through a permission lattice: hard-deny → deny rules → ask rules → allow rules → per-mode defaults. Headless runs never auto-approve; a needed approval is a non-zero exit, not a guess. |
| **Real shell analysis** | scripts are parsed with a bash AST and every command through pipes, `&&`, subshells, loops and `sh -c '…'` is classified (read / mutating / network / destructive / privilege). Opaque constructs — `eval`, `$(…)`, `sh -c "$X"`, unknown binaries — can **never** match an allow rule. |
| **Hard-deny floor** | in every mode, including bypass: `sudo`/`su`, `curl … \| sh`, decode-to-shell, `rm -rf` of `/`, `$HOME` or the workspace, force-pushing `main`, `git config core.hooksPath`, reading credential files, writing to `~/.ssh`, `.git/hooks`, rc files. |
| **OS sandbox** | shell commands run under bubblewrap or Landlock (Linux) or sandbox-exec (macOS, v0.1.x): workspace read-write, system read-only, `$HOME` hidden, network off unless granted per command. When no backend is available the status bar says `sandbox off` in red and bypass mode is refused. |
| **Secrets stay local** | `.env`, `*.pem`, `id_*`, `credentials*` … are refused to every tool; provider keys and anything named `*_TOKEN`/`*_SECRET`/`AWS_*` are stripped from the sandbox environment; secret-looking strings in tool output are replaced with `[redacted: github…3f9a]` before the model sees them. |
| **No telemetry** | wright opens no network connection except to your chosen model provider, MCP servers you configured, `web_fetch` targets you approved, and a loopback Ollama probe. No crash reporting, no update check unless you enable `updates.check`. A test in `internal/app` enforces this at the import level. |
| **Auditable** | every call, verdict, file change, command and model call is appended to a SHA-256-chained JSONL log per session (`wright audit verify`), with an honest end-of-run summary. |
| **Honest UI** | deny is as easy as allow; the cursor defaults to *deny* for destructive, opaque, network and MCP requests; "always allow" is never offered for destructive or opaque commands and never wider than two argv words. |

## Install

**Homebrew** (macOS and Linuxbrew):

```sh
brew install --cask richardwooding/tap/wright
```

**Go:**

```sh
go install github.com/richardwooding/wright/cmd/wright@latest
```

**Container** (the image carries git, bash, coreutils and ripgrep; the
container is the sandbox boundary):

```sh
podman run --rm -it -v "$PWD:/workspace:Z" -e ANTHROPIC_API_KEY ghcr.io/richardwooding/wright
```

Or download a prebuilt binary for macOS or Linux from the
[releases page](https://github.com/richardwooding/wright/releases). It is a
single static binary — no cgo, no runtime dependencies. `git`, `rg` and
`bwrap` are used when present; `wright doctor` shows what was found.

## CLI

```sh
wright                          # interactive session in the current repo
wright "add tests for the parser"
wright -p "explain the build" --output json     # headless, exit 3 if an approval was needed
wright --mode plan              # read-only: edits and mutating commands are denied
wright --allow 'bash(npm test *)' --add-dir ../shared
wright doctor                   # sandbox backends, landlock ABI, git, rg, which provider keys are set
wright config paths             # where settings, data and cache live
wright audit verify             # check the session log's hash chain
```

| flag | meaning |
| --- | --- |
| `-p, --print` | headless mode; `--output text\|json\|stream-json` |
| `-m, --model` | model name (`WRIGHT_MODEL`); otherwise detected from the env keys present |
| `--mode` | `default`, `plan`, `auto-edit` (`WRIGHT_MODE`); `bypass` only via `--bypass-permissions` |
| `--sandbox` | `auto`, `bwrap`, `landlock`, `seatbelt`, `none` (`WRIGHT_SANDBOX`) |
| `--allow` / `--deny` | permission rules for this run (repeatable) |
| `--add-dir` | extra directory the agent may access (repeatable) |
| `--allow-network` | let sandboxed commands reach the network without asking |
| `-r, --resume` / `-c, --continue` | resume a session by ID, or the latest |
| `--plain` | no alternate screen (implied by `NO_COLOR`, `TERM=dumb`, non-TTY) |

Exit codes: `0` ok · `1` provider/tool error · `2` usage · `3` approval
required · `4` budget exhausted · `130` interrupted.

## Permission rules

Rules live in `~/.config/wright/config.json`, `.wright/settings.json`
(shared) and `.wright/settings.local.json` (yours, gitignored), under
`"permissions": {"allow": [...], "ask": [...], "deny": [...]}`, and on the
command line with `--allow`/`--deny`.

```
rule    := tool [ "(" spec ")" ] [ "+net" ]
tool    := read_file | write_file | edit_file | glob | grep | list_dir | bash
         | web_fetch | web_search | explore | skill
         | "mcp:" server [ ":" toolglob ] | "*"
spec    := pathglob                 doublestar; relative = workspace-relative; "~/" and "$WORKSPACE/" expand
         | argv-prefix [ "*" ]      bash: matched per simple command after AST split
         | "re:" RE2                bash, advanced
         | "domain:" host | "*.host"   web_fetch
```

```json
{
  "permissions": {
    "allow": ["bash(go test *)", "bash(npm run lint)", "edit_file($WORKSPACE/internal/**)", "web_fetch(*.pkg.go.dev)"],
    "ask":   ["bash(git push *)"],
    "deny":  ["*(**/*.tfstate)", "bash(docker *)"]
  }
}
```

A shell script is allowed by rules only when **every** command in it matches an
allow rule and nothing in it is opaque. `*` is refused in allow lists. Project
`allow` lists (and `additionalDirectories`, `sandbox.passEnv`, `mcpServers`)
are inert until you accept the project once; project `ask`/`deny` always
apply, because tightening is free. Bypass mode is a flag, never a setting,
and the hard-deny floor applies to it too.

## Settings

`~/.config/wright/config.json` < `.wright/settings.json` <
`.wright/settings.local.json` < environment (`WRIGHT_MODEL`, `WRIGHT_MODE`,
`WRIGHT_SANDBOX`, `WRIGHT_CONFIG_DIR`, `WRIGHT_DATA_DIR`) < flags. Sessions,
snapshots and audit logs live under `$XDG_DATA_HOME/wright/projects/<hash>/`.
Commits made by the agent carry `Co-Authored-By: wright <wright@richardwooding.github.io>`
by default (`git.attribution`, `git.trailer`).

## Status

Phase 0/1 of the plan: scaffold, theme, config, workspace, shell classifier,
policy engine, redaction, audit, snapshots, trust store, git helpers, sandbox
backends and `wright doctor`. Next: tools, the agent engine, the TUI and
headless mode, then sessions, MCP and skills. See [CHANGELOG.md](CHANGELOG.md).

## License

[MIT](LICENSE)
