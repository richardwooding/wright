# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- **The transcript scrolls.** Bare up/down now move it at the composer's
  edges, which is also what makes the **mouse wheel** work: on a VTE terminal
  the wheel in the alternate screen *is* those keys, and the composer was
  eating them for prompt history. History moves to `ctrl+p`/`ctrl+n`; `home`
  and `end` jump to the top and back to following; the status bar says when
  you are scrolled back, because otherwise new output silently stops
  appearing. An approval arriving mid-run no longer yanks you to the bottom —
  mid-run is exactly when you are reading back, and the prompt is modal
  anyway.
- **The window title animates while a run is in progress.** It said "running"
  with a dot that never moved, because nothing recomputed it between state
  changes.
- **`/github on` asks how long**, instead of printing a snippet to paste: this
  session, this project, or every project. Both persistent answers are written
  to *your* config, never the project's — `.wright/settings.json` is
  committed, so a project-scoped `github.auth` would be asking everyone who
  clones the repository to hand over their own credential.
- **The diagnostics endpoint can take a prompt**, once `/debug inject on` arms
  it with a typed confirmation. `POST /debug/input` with the token it prints,
  and the prompt enters the session — marked "via the debug endpoint" in the
  transcript, prefixed in the text the model sees, and recorded in the audit
  log. It is off by default, ends with the session, and is never written to a
  file. The handler never blocks on the session: a full buffer is a 503 you
  can see rather than a wait you cannot.

### Fixed

- **A web page could read your session's diagnostics.** The endpoint had no
  `Host` check, so a page whose DNS rebinds to 127.0.0.1 became same-origin
  and could fetch `/debug/state` — goroutine stacks and the commands the
  session had run. The Host must now be the loopback address being served, and
  requests carrying browser headers (`Origin`, a cross-site `Sec-Fetch-*`) are
  refused outright. This affects any session run with `--debug-addr`; there is
  no configuration to change.
- Every endpoint route accepted any method, because they were registered as
  bare paths. They are method-qualified now (`pprof/symbol` keeps the POST it
  documents).
- A settings file written before v0.3.4 can hold duplicate rules that nothing
  would ever have removed. Saving a grant now cleans them, preserving order.

### Security

- Arming the endpoint's input is a real capability: anything that can reach
  the port *and* present the session's token can drive the agent. The defences
  are that it is off unless a human typed a word at the terminal, that
  `application/json` is required (which forces a CORS preflight this endpoint
  never answers, and rules out a `<form>` outright), that the token is minted
  per arming and dies when you disarm, and that every prompt is visible and
  audited. The token is not a boundary against code already running as you —
  same-user code can read this process's memory — and the docs say so.

## [0.5.1] - 2026-09-20

### Fixed

- **`git remote add` failed with "Device or resource busy" and no explanation.**
  wright bind-mounts `.git/config`, `.git/hooks` and `.wright` read-only for
  the whole session — a hook or config key written inside would run *outside*
  it, on the user's next git command — and a bind-mounted file reports EBUSY
  rather than EROFS, so none of the existing read-only detection caught it.
  bash now recognises it and says what happened, that it cannot be worked
  around from inside, and that `git push <url> <refspec>` needs no remote.
- **The model is told the git rules up front**, in the bash tool's
  system-prompt guidance: which paths are read-only, and that it must never
  pass `-c credential.helper` or set `GH_TOKEN` itself, because a session with
  GitHub authentication already gives networked calls both — and a `-c` that
  names a program is refused by the policy engine. Both halves were learned
  the expensive way in a real session, which spent two turns and a denied call
  rediscovering them.

## [0.5.0] - 2026-09-20

git and the GitHub CLI, made to work. Both were *allowed and then broken*
inside the sandbox — the failure this project has a name for — and the
evidence came from a live session: `gh auth status` reported "You are not
logged into any GitHub hosts" **after** the user approved it and paid for a
network grant.

### Added

- **`--github-auth`, `/github on`, or `"github": {"auth": true}`** lets shell
  commands that run with network access authenticate to GitHub as you. wright
  takes a token from `GH_TOKEN`/`GITHUB_TOKEN`, or asks `gh auth token` **on
  the host**, and gives it — together with git's credential helper, pointed at
  gh by absolute path — to those calls and no others. `git push` over HTTPS
  and every `gh` command work; both were impossible before.

  It is **off by default**, and every way in is the user's own: the flag has
  no environment variable (an `.envrc` a repository ships must not be able to
  turn it on), the settings key is honoured from your user config only and
  ignored from project settings *even when trusted*, and `/github on` asks for
  a typed confirmation that spells out what every networked command will then
  carry.

  The control is that **a command with no network never receives the
  credential** — not redaction, which a `base64` defeats. An auto-allowed read
  runs with no prompt at all, and it has no token in its environment. The
  token is not in the base sandbox spec either, so the MCP stdio servers a
  session starts never see it.
- **A grant scope that covers every project.** A rule saved for one repository
  had to be saved again in the next, and for git and gh that is most of the
  friction. The approval prompt now offers a third scope, written to your user
  config. It is last, never focused, and its label names the file.
- `/debug` and the diagnostics dump report whether the session is
  authenticated and where the token came from — the source's name, never the
  value. The approval prompt gains a line on the calls that carry it, and the
  audit log records `granted_github`.

### Fixed

- **`gh` was classified by substring**, which was wrong in both directions:
  `gh pr create --delete-branch` escaped being read as a deletion only because
  the option happened to be dropped, and deletions were recorded as needing no
  network — so an approval of one granted none. It is now a noun/verb table
  like git's, with `gh api` classified by its method.
- **Four kinds of `gh` command join the hard-deny floor**: deleting a
  repository, `gh api -X DELETE`, anything touching stored credentials (auth,
  secret, ssh-key, gpg-key) and anything that installs or names code to run
  (`alias set`, `extension install`). This closes a real hole: the only
  `Destructive` guard in the policy layer is in *offering* a rule, never in
  matching one, so a broad `bash(gh *) +net` — which one session saved,
  because its first `gh` command was `gh --version` — used to auto-allow all
  of them.
- **A value-taking option could carry a command past the floor.**
  `gh --repo o/r repo delete` parsed as the noun "o/r" and matched nothing.
  The parser now skips those options and their values, and the floor is
  checked separately over adjacent words, so an option this table has not been
  taught about can shift the parse without ever losing a hard deny.
- **The offered rule names the command group.** The classifier knows gh's
  shape, so one approval of `gh pr view` covers `gh pr list` and nothing
  wider, instead of the broadest rule the grammar allows.
- Config keys that matter once a credential can exist are neutralised:
  `core.askpass`, `http.proxy` and `core.gitproxy` (a cloned repository's own
  proxy would otherwise be a way to receive an authenticated request), and
  `GIT_TERMINAL_PROMPT=0` so a command needing a credential fails instead of
  blocking on a prompt nobody can answer.
- A diagnostics endpoint could panic the process: the serve goroutine read the
  server field that `Close` had already cleared.

### Security

- This hands a live GitHub credential to a model-driven process. That is what
  it is for, and the reasons it is opt-in, per call, trust-independent and
  visible. The limit, stated plainly rather than implied: any command that
  gets the token **and** the network can do anything the token can, including
  send it somewhere. Redaction is a backstop — it keeps the token out of the
  transcript, the audit log and your screen — not a boundary.
- **SSH remotes still cannot authenticate**, and this does not change that.
  Forwarding an agent socket would hand the sandbox every key you hold, with
  no way to scope it.
- Under bwrap the environment is passed on the command line, so while a call
  runs its token is visible in `/proc` to processes running as you.

## [0.4.0] - 2026-09-20

Debugging a live session. Asked for after a session that stopped dead with no
error and nothing in the transcript: the only way to find out what it was
waiting on was to read the audit log and poke at `/proc`.

### Added

- **`kill -s SIGUSR1 <pid>` writes a report** of what the session is doing —
  model, mode, sandbox, the approvals waiting for an answer, the tool calls
  that have not returned, background jobs, and every goroutine's stack — to a
  file under the session's own directory. It needs no port, no event loop and
  no working UI, which is exactly the case it exists for. `/debug dump` does
  the same from inside the TUI.
- **`--debug-addr 127.0.0.1:6060` serves the same report** at `/debug/state`,
  with `net/http/pprof` beside it, for watching a live session from another
  terminal. It is off by default, and it is a flag and nothing else: no
  environment variable and no settings key, so a repository cannot open a port
  on the machine of anyone who runs wright in it. Only a loopback IP is
  accepted — `0.0.0.0`, a bare `:6060` and `localhost` are all refused — and
  a bad or busy address fails the session at startup rather than in a
  goroutine.
- The endpoint's address is **in the status bar** while one is listening,
  beside the sandbox, and a startup warning names it: a process serving its
  own stacks and a CPU profiler to everything running as this user is not a
  state to leave someone to discover. **`/debug`** has the rest — the pid to
  signal, where the dumps go, and the last one written.
- Dumps and the endpoint carry the same redaction as tool output, because a
  report quotes the commands a session ran. Deleting a session deletes its
  dumps.

### Security

- This is the first part of wright that listens on a socket. It never dials:
  `TestNoUnexpectedNetwork`, which backs the "no telemetry, no phone-home"
  claim, now allows `internal/diag` to import `net/http` with that reason
  written down beside it. Note the asymmetry worth knowing about: `web_fetch`
  is hard-denied from loopback addresses, and a sandboxed `bash` under
  bwrap or landlock sits in its own network namespace and cannot reach the
  port — but `--sandbox none` has no such barrier, so under it a command the
  agent runs could read the endpoint like any other local process.

## [0.3.4] - 2026-09-20

The other three ways the approval prompt defeated itself, from the same
Pascal session: a trailing `echo "exit=$?"` made a whole script un-allowable,
the rule offered named the `timeout` wrapper instead of the program, and one
prompt could only ever accept one rule.

### Fixed

- **`echo "exit=$?"` made an entire script impossible to allow.** Any word
  that depends on runtime expansion marked the script *opaque*, and no allow
  rule may match an opaque script — so the same build asked for approval with
  two rules offered when it ended `… | tail -60`, and with **none** when it
  ended `… | tail -70; echo "exit=$?"`. The agent writes that idiom
  constantly; every one of them cost a rule.

  A dynamic *argument* now only hides the script when the command could act on
  the value. Inertness is declared by the command's own handler — `echo`,
  `printf`, `true`, `pwd` take no file operands at all — never inferred, and a
  redirect disqualifies: `echo "$X"` is legible, `echo "$X" > f` is not.
  Everything else is unchanged: `rm $X`, `cat $F`, `$TOOL`, `sh -c "$CMD"`,
  `timeout $N cmd`, `export PATH=$X` and `GOFLAGS=$F go build` all stay
  opaque.
- **The rule you were offered named the wrapper, not the program.** For
  `timeout 120 ./bin/llmkittests --all` the offer was `bash(timeout 120 *)` —
  at once too broad (any command under that exact wrapper) and too narrow
  (`timeout 30 …` never matched it), and the binary's own rule was never
  proposed. An offer now names the program a wrapper peels down to, and a rule
  matches a command by either spelling, so `bash(./bin/llmkittests *)` covers
  every duration and a rule already saved for the outer form keeps working.
  Every floor still runs first: `timeout 120 rm -rf ~` is hard-denied with a
  rule for `bash(rm *)` present.
- **Accepting the same rule twice saved it twice.** One user's
  `settings.local.json` held three copies of `bash(fpc *)`.

### Added

- **One prompt can now accept several rules.** A script routinely needs a rule
  per command — `cd X && fpc … && ./bin/t --all` wants two — and accepting one
  meant being asked again on the very next call. On the "allow…" page, space
  marks a rule and enter applies everything marked; enter with nothing marked
  still applies the focused row and a number still applies that one row
  immediately, so the single-rule case costs exactly what it did before. Rows
  show `[x]`/`[ ]` rather than relying on colour, and `--plain` takes a
  comma-separated answer (`2,4`). Marking both scopes of one rule saves it
  once, at the wider scope.
- The bash tool now tells the model it already has a timeout — 120 seconds by
  default, 600 at most, the whole process group killed — in its description
  and in the system prompt, so the agent stops wrapping commands in `timeout`
  itself. The sentence is built from the constants that enforce it. The bound
  is per call, so there is still no way to bound one command inside
  `a && b | c`, and a harness timeout reports exit code -1, not 124.

### Changed

- Audit `decision` lines record `grants` (a list) instead of `grant`; an
  export of a session recorded by an earlier wright still shows what was
  saved.
- Matching the program a wrapper peels down to widens what an existing rule
  covers: `bash(rm *)` now also covers `timeout 5 rm …`. The hard-deny floor,
  the containment checks and the secret/protected paths all still run first.

## [0.3.3] - 2026-09-20

"Always allow" now works for programs wright has never heard of. Reported from
a session compiling Pascal, where the same `fpc` command was approved five
times because no rule could ever have covered it.

### Fixed

- **A program wright has no description of could never be allowed — you were
  asked on every single call, for ever.** A script mentioning any command
  outside the classifier's table was treated as *opaque*, the same as
  `eval "$CMD"`, and opaque scripts match no allow rule by design. So no
  "always allow" was offered (it could not have been honoured), and writing
  `bash(fpc *)` into settings by hand did nothing either. A user compiling
  Pascal approved the same command five times in one session with no way to
  stop it.

  Being unreadable and being unfamiliar are now different things. A script the
  analyser cannot see through — a dynamic command name, `eval`, `sh -c "$X"`,
  a call to a function the script defines — is unchanged: no rule may ever
  match it. A program that is merely absent from the table is fully legible,
  so it still asks by default and is still assumed to change the workspace,
  but a rule naming it covers it. That makes "always allow" work for `fpc`,
  and for cargo, zig, tsc, dotnet, mvn and everything else nobody has
  modelled.
- Such a command was also reported as **destructive**, with *deny*
  pre-selected — the treatment meant for `rm -rf`. Compiling a file now reads
  as the mutating command it is, and the prompt names the program it does not
  recognise instead of claiming the script is unreadable.
- A saved rule was defeated by the glue around it. Coverage needs every
  command in a script matched, so `cd dir && tool … | grep …` kept asking even
  with a rule for `tool`. A command that would run with no prompt on its own —
  a safe read declaring no paths — no longer needs a rule of its own, and is
  no longer offered one.
- An offered rule pinned the first *flag* it saw (`bash(fpc -Mobjfpc *)`),
  which stopped matching the moment the model reordered its flags, so the
  rule you accepted quietly stopped working. Offers now pin a second word only
  when it is a subcommand: `bash(git push *)`, but `bash(fpc *)`.
- A malformed function declaration (`()0`) crashed the shell analyser. Found
  by the fuzzer while checking the above.

## [0.3.2] - 2026-09-20

Resuming a session now continues it: same permission mode, recorded when you
change it rather than when a run happens to finish.

### Fixed

- **Resuming a session lost its permission mode.** A session switched to plan
  and picked up later came back in default mode — silently, which is the wrong
  direction for a permission mode to move on its own. A resumed session now
  starts in the mode it was left in and says so; an explicit `--mode` or
  `WRIGHT_MODE` still wins.
- **A mode or model change was only recorded when a run finished.** Switching
  with `shift+tab` before typing anything — the usual moment — was lost if that
  run never completed, so even the record of the mode was wrong. Both are
  written as soon as they change.

## [0.3.1] - 2026-09-20

A session that stopped dead with no error, reported from a real run. Two tool
calls needed approval at once and only one prompt could be on screen, so the
other was never answered and its step never finished. `/ps` is the other half
of that report: a way to see what a quiet session is actually doing.

### Fixed

- **A session could stop dead when two tools needed approval at once.** Tool
  calls run in parallel, so one step can raise several approval prompts, and
  each has a call waiting on its answer — but the overlay was a single slot
  that each new prompt overwrote. The replaced prompt was never shown and
  never answered, so its tool call blocked the step forever: no error, no
  prompt, no further output, and nothing written to the transcript because
  the step never completed. Prompts now queue, and a run's prompts are
  dismissed when the run ends rather than lingering over a call that is
  already over.

### Added

- **`/ps`** — what the session is doing right now: the tool calls in flight
  and how long each has been running, or that the run is waiting on the model,
  plus any prompts queued behind the one on screen. A spinner does not say how
  long it has been spinning, and a run waiting on the model looks exactly like
  a run waiting on a command.

## [0.3.0] - 2026-09-20

A session you are sitting in is now a session you can read. v0.2.1 made it
exist from the moment it starts; this makes its transcript arrive as the run
produces it, rather than when the run is over.

### Changed

- **A session's transcript is now written as the run happens**, one completed
  step at a time, rather than all at once when the run ends (agentkit v0.4.0).
  `/export` during a long run shows the tool calls that have finished instead
  of a bare header, `/sessions` shows the session with its title from the
  first turn, and a run that is interrupted keeps the steps it completed. Only
  the window before the first step finishes is empty, and the export says so.

## [0.2.1] - 2026-09-20

Three bugs from a real v0.2.0 session, all of them wright contradicting
itself: a mode that removed tools its own policy allowed, an offer that
could not be honoured, and a session that did not exist while you were
sitting in it.

### Fixed

- **Plan mode removed tools the model was told it had.** The toolset plan
  mode keeps was the *explore sub-agent's* read-only list, so `web_fetch`,
  `web_search`, `bash`, `todo_write`, `ask_user` and `job` were dropped —
  while the system prompt still described them. Calling one came straight
  back as `tool not found`, with no approval prompt and no explanation. The
  policy was never the problem: plan mode allows read-only shell commands
  and asks for web access. Plan mode now keeps every tool its own policy can
  permit, and a test pins the two lists together.
- **Plan mode suggested rules that cannot work there.** It never consults
  allow rules, but it still offered them: the approval overlay invited
  "always allow", which then kept asking, and a headless denial named a
  `--allow` flag that changed nothing. Plan mode now offers no saved rule and
  says what would actually help.
- Exporting a session whose first run is still going produced a header and
  nothing else, which reads as lost data. It now says the transcript is
  written when a run finishes.
- **A session did not exist until its first run finished.** Both the sidecar
  and the transcript were written only at the end of a run, so during the
  first one `/sessions` listed nothing and `/export` failed with "no such
  session" — naming the very id in the status bar. The session is recorded
  when it starts.

## [0.2.0] - 2026-09-20

Trust, and what a grant is allowed to hide. wright now asks whether you trust
a directory before it reads anything in it, and in exchange stops asking about
every ordinary edit inside it. The same question runs through the rest of the
release: an exported transcript finally shows the approval decisions, the
audit log records what an approval actually handed over, a saved rule can
carry an install's write grant, an MCP server can be accepted from a prompt,
and the system prompt no longer describes tools that were never registered.

### Added

- Exported transcripts (`wright sessions export`, `/export`) now show the
  approval decision between each tool call and its result: who decided, the
  outcome, the rule and its source, the reason, and what allowing granted
  (network, and any writable paths). A policy allow renders as a single line
  so an auto-allowed `read_file` stays out of the way; a user's answer and
  every denial get a full block. Sub-agent decisions are left out — their
  calls are not in the transcript — and a session with no readable audit log
  still exports, with a note saying decisions are unavailable.
- `web_search` is wired to a configurable provider (`search.provider`, Brave
  today) and is finally callable. With no provider named the tool is not
  registered and no query is ever sent anywhere; the API key comes from the
  environment (`BRAVE_API_KEY`) and is never echoed in an error. A provider
  named by an untrusted project's settings is ignored, because a search
  discloses the query to a third party.
- **Background `bash` jobs.** A `bash` call with `background` set returns a
  job id at once; the new `job` tool lists jobs, reads a job's new output
  since it was last read, or kills one, and `/jobs` shows the same list. The
  approval is the same approval, and the prompt says the call runs in the
  background before you answer, because the grant it earns lasts as long as
  the job does. Jobs are killed when the session ends: one left running would
  hold a grant nobody could see or revoke.
- **The interactive MCP consent prompt.** An unaccepted server was reported
  in full and then declined, because there was no form to ask with. An
  interactive session now asks, after showing the server's command and every
  tool it offers: do not connect, connect for this session only, or connect
  and remember it. Only the last records anything. Anything that is not one
  of the three — no answer, no terminal, `-p` — still declines, so a server
  is never connected because a question could not be asked.
- **`+install` rule flag**: `--allow 'bash(brew install *) +install'` grants
  an installing command the network *and* the package-manager prefixes for
  that one call — the same grant an interactive approval makes, so an
  approval can now be saved as a rule. Previously only `+net` could be
  written down, and an unattended install was allowed and then failed on a
  read-only file system; a rule with only `+net` now falls through to a
  prompt that can grant it instead.
- **`multi_edit`**: several edits in one call, across files or repeatedly in
  one, each seeing the previous one's result. Every edit is validated before
  any is written, so a call that cannot be applied in full changes nothing;
  if a write still fails part-way, the files already written are put back and
  the error says so. It asks for approval like any other write, and the
  approval prompt shows a diff of the whole call.
- The window (and process) title says when wright is working: `● wright — <dir>`
  while a run is in flight, `wright — <dir>` when it is idle, so a session left
  working in another tab is visible from the window list. The title is also set
  before the first terminal size arrives, which previously left the window
  unnamed for a frame.
- A horizontal divider above **and** below the input box, separating the
  composer from the transcript and from the status bar.
- `shift+up` / `shift+down` scroll the transcript a line at a time — the
  keyboard replacement for the wheel, which is off by default (below).
  `pgup` / `pgdn` still page.
- `alt+m` and `/mouse` toggle mouse tracking for the session.
- **Workspace trust.** The first interactive session in a directory asks
  whether you trust it, before anything starts; declining exits with the new
  code `5` and starts nothing. In a trusted directory, editing files inside
  it no longer prompts — and nothing else changes: shell commands, the
  network, paths outside the directory, sensitive files (`*.tfvars`),
  ignored files and the whole hard-deny floor still ask or refuse, and plan
  mode still allows no edits. The grant is applied in the permission mode
  table, after every one of those checks, never as an allow rule.
- `--trust` answers that question in advance for a wrapper script, and
  `wright trust list|accept|forget` shows what has been accepted and takes it
  back — nothing could undo trust before.
- `wright config paths` now lists `trust.json`, the file that decides both
  whether a directory's edits prompt and whether its settings apply.

### Changed

- **The mouse is off by default, so your terminal's own text selection works
  again.** Mouse tracking was only ever used for wheel-scrolling the
  transcript, and it cost click-and-drag selection and copy everywhere in the
  window. Turn it back on for a session with `alt+m` or `/mouse`.
- `/help`'s key table groups related keys on one row; the overlay is as tall as
  the transcript and the two new divider rows made it shorter.
- A first run in a directory that also ships `.wright/settings.json` asks one
  question covering both, instead of two.
- Headless (`-p`) is deliberately unchanged: it never asks the trust
  question, never exits over it and never takes the baseline, whatever was
  accepted in a terminal, so an unattended run behaves the same everywhere.
- `trust.json` records the workspace acceptance in its own field, independent
  of the settings hash: editing `.wright/settings.json` no longer has any
  bearing on whether the directory is trusted, and vice versa. Records
  written by earlier versions read as "never asked".

### Fixed

- Exporting a session that does not exist wrote a transcript header for it
  claiming "unknown model" instead of reporting that there is no such
  session.
- The audit log could not say whether the user allowed or denied a call. Both
  answers were recorded as the verdict that raised the prompt (`ask`, by
  `user`), so an allow and a deny read identically and the end-of-run summary
  counted a denial as merely asked. The recorded outcome is now the user's
  answer; `Summary.Asked` still counts prompts shown, and `wright audit show`
  names who answered.
- A headless denial was recorded as `ask`. Headless never prompts and always
  denies, so a CI run's log — the one most likely to be read by someone who
  was not there — claimed nothing had been denied. The verdict that would
  have prompted is still legible in the reason, which names the `--allow`
  rule that would let the call through.
- The audit log never recorded what an approval handed the call. A decision
  now carries `granted_network` and `granted_writable`, so the log can show
  that a command was allowed but ran *without* the network — the fact a
  confusing session could not explain. Logs written before this read as
  nothing granted.
- The system prompt described `web_fetch`, `web_search`, `todo_write` and
  `ask_user` whether or not they were registered. A model told a tool exists
  calls it and reads "unknown tool"; the guidance is now filtered to the
  toolset it was built beside.
- **auto-edit mode did nothing.** The builtin `edit_file($WORKSPACE/**)` ask
  rule decided one step above the mode table, so with the builtin layer
  loaded — which is to say, in every real run — `--mode auto-edit` asked for
  every edit exactly like default mode, while the README's mode table said it
  allowed them. Edits inside the workspace now run without asking, and the
  sensitive-file, ignored-file, outside-the-workspace, `.git`, `.wright` and
  secret floors all still fire, as does any ask rule you wrote yourself.
- A rule could mean one thing and print another. `bash(\rre:()` was read as
  an argv rule (the regex-prefix check trimmed only spaces and tabs, while
  the argv branch trimmed all whitespace) and then rendered as
  `bash(re:()` — a rule a permissions list or an audit log could not
  describe. Both branches now trim the same whitespace. Found by the rule
  fuzzer; it predates v0.1.3.
- `/agents` was answered by the app but never offered by the TUI, so typing
  it said "unknown command" and completion never suggested it.
- An overlay on a very short terminal no longer pushes the composer and status
  bar off the screen: the overlay box has a minimum height and was not clipped
  to the space it was given.

### Internal

- `config.Merge` is a hand-written field list, so a field added to `Settings`
  was silently ignored by every layer above the embedded defaults. A
  reflection test now fails if any field lacks a merge rule.

## [0.1.3] - 2026-09-20

From a user's session log: the sandbox's own restrictions were invisible to
the agent reasoning inside them, so a command wright had deliberately
confined looked like a broken machine. It now says which restriction caused
the failure and what grants it.

### Fixed

- `brew update`, `update-reset`, `pin` and `unpin` rewrite Homebrew's own git
  repositories under the prefix, so they need the writable prefix an install
  does. They were classified as ordinary network commands and failed on a
  read-only mount.
- A command that fails *because of the sandbox* now says so. When a call that
  ran without network access reports a connection failure, or a write reports
  a read-only file system, the result explains which restriction caused it and
  what grants it. A user's session showed the agent spending ten tool calls
  rediscovering that its own sandbox had no network, and still reaching the
  wrong conclusion.

## [0.1.2] - 2026-09-20

An approved call now gets what it needs to work. A user reported that
`brew install` could not succeed even after they allowed it: the command
needed the network and a writable Homebrew prefix, and approving granted
neither, so the agent was left reporting a failure the user had already
said yes to.

The sandbox was strict in a way that made ordinary work impossible: the user
allowed `brew install`, and it still could not run. A harness that cannot do
ordinary work is not secure, it is broken.

### Changed

- **Approving a call now grants what that call needs.** When the classifier
  says a bash command needs the network, allowing it runs it with the
  network, and the prompt says so before you answer. The separate "allow with
  network" option (`w` in the TUI and in `--plain`) is gone: offering it for
  a command that cannot work without the network meant plain "allow" produced
  a call that failed after the user had said yes — which is what happened to
  `brew info fpc` ("curl: (7) Could not connect", with the status bar showing
  `bwrap ⊘net`). The two invariants around it are unchanged: the model's own
  `network: true` argument never self-grants (it turns an allow into an ask,
  and the engine rewrites the argument to the verdict), and a *persisted*
  rule still needs an explicit `+net` to carry the network.
- A command the classifier identifies as installing software (`brew install`,
  `go install`, `cargo install`, `npm install -g`, `pipx install`, `gem
  install`, `rustup update`, …) gets the tool prefixes it writes — the
  Homebrew prefix and cache, the npm global prefix, `CARGO_HOME`,
  `RUSTUP_HOME`, `$GOBIN`, `~/.local/bin`, the uv and pipx directories and the
  gem home, whichever exist — mounted read-write **for that one call**, and
  only when the user approves it. The approval prompt names the exact
  directories, so the consent is to a specific list. The base sandbox is
  unchanged: nothing outside the workspace is writable by default, no saved
  rule and no mode widens it, and `$HOME` and `/` are never bound.

### Fixed

- Package-manager verbs that use the network were classified as local reads,
  so they ran with the network off *and* with no prompt at which to ask for
  it. `brew info`, `brew deps`, `brew outdated` and `brew doctor` are network
  commands (only `list`/`ls`, `config`, `leaves`, `--prefix` and `--version`
  are local, and the option-spelled ones are now recognised at all — the verb
  lookup only ever saw positional words, so `brew --version` fell through to
  the network default). The same audit fixed `npm doctor` and `npm ping`
  (both contact the registry), `pip list --outdated` and `gem list --remote`
  (both query the index), and `dnf`/`yum`/`snap`/`flatpak`/`rpm-ostree`
  queries, which refresh metadata from a remote index — while `apt`, `pacman`,
  `apk`, `zypper`, `rpm` and `port` answer from the index on disk and stay
  local.
- `pnpm global add` / `yarn global add` were classified as an unknown verb
  (`mutating`) instead of a global install.

## [0.1.1] - 2026-09-20

Four defects found by CI's macOS job and by following its lead on Linux.
Three are security-relevant and all four share one cause: a fixed list of
paths compared against values that have already been symlink-resolved.

### Security

- A recursive `chmod`/`chown` on a system root escaped the hard-deny set when
  the root was a symlink. Paths reach the classifier already resolved, and
  most modern Linux distributions point `/bin`, `/sbin` and `/lib` at `/usr`,
  while macOS points `/etc`, `/var` and `/tmp` into `/private` — so
  `chmod -R 777 /bin` was seen as an ordinary write to `/usr/bin` and was
  allowed outright in bypass mode. Both spellings now match.
- Protected paths were not protected on macOS. `/etc`, `/var` and `/tmp` are
  symlinks into `/private` there, and every path in a permission request is
  symlink-resolved, so `/etc/passwd` reached `workspace.IsProtected` as
  `/private/etc/passwd` and the literal protected set did not match it:
  reading it was merely "outside the workspace" (an ask), and in
  `--bypass-permissions` it was allowed outright — a hole in the hard-deny
  floor that no mode is supposed to have. `workspace.Open` now records the
  symlink-resolved spelling of every protected location as well as the
  literal one, so the floor holds for the system directories, wright's own
  config directory, and credential directories and rc files under `$HOME`
  that a dotfiles repository (or a home directory under a link) reaches
  through a symlink. Linux is unaffected, where the two spellings are the
  same.

### Fixed

- Project trust was keyed by the spelling of the project's path, so a
  project accepted once could stay untrusted for ever. Acceptance recorded
  one spelling and the check looked up another (resolved versus not), which
  on macOS — where a project under `/var/folders` resolves to
  `/private/var/folders` — broke project trust outright: `.wright/settings.json`
  never applied, however often it was accepted. `trust.Store` now records and
  looks up projects under the absolute, symlink-resolved path, and finds a
  record written under either spelling.

- `grep` with a limit could drop its truncation note. Ignored and hidden
  files were filtered out *after* the limit was applied, so a match ripgrep
  happened to emit first from an ignored directory consumed the limit and
  then vanished, leaving too few matches to report that more existed.
  ripgrep's file order varies by version, which made it look like a flake.
  The visibility filter now runs inside the reader, so the limit only ever
  counts matches the user will see.

## [0.1.0] - 2026-09-20

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

- A repository carries its own `.git/config`, so cloning a hostile one and
  running an ordinary `git diff` or `git status` executed whatever
  `diff.external` or `core.fsmonitor` named — with no prompt, because those
  commands are allowed by default and the command line is innocent. wright
  now overrides the configuration keys that name a program
  (`diff.external`, `core.fsmonitor`, `core.sshCommand`, `credential.helper`,
  `core.editor`, `sequence.editor`) in the environment it gives git, which
  takes precedence over every config file including the repository's.
- Plan mode no longer consults allow rules. They match on tool and argv,
  never on effect, so the builtin `bash(git show *)` authorised
  `--output=<path>` and a user's `bash(go build *)` authorised a build in the
  one mode that promises not to change anything. The mode table still allows
  reads, which is all plan mode is for.
- The sandbox's environment strip list and the classifier's list of
  code-injecting build variables are one list. They had drifted, and a
  variable the classifier treats as a payload could still be forwarded into a
  sandboxed command.
- A recursive reader given no path operand reads the working directory while
  naming nothing, so the credential scan had nothing to look at: `grep -r ""
  .` asked, but `grep -rI SECRET` returned `.env` to the model with no
  prompt. The scan now treats the working directory as an implied read for
  the readers that default to it.
- Commits made inside the sandbox were attributed to `user@hostname`. Both
  backends put the user's global git config out of reach — bwrap replaces
  `$HOME` with a tmpfs, landlock grants it no rule — so git invented an
  identity, and under landlock an unreadable `~/.gitconfig` made `git commit`
  fail outright. wright now resolves the commit identity on the host, where
  `includeIf` still applies, and carries it in, so a commit is attributed to
  the address the user actually configured or is not made at all.
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
- `git bisect run <cmd>` runs a program of the caller's choosing at every
  step of the search and was classified safe-read, so it ran with no approval
  at all. `bisect` now has a handler: `run` is opaque and privileged,
  `replay` declares the log file it reads, the subcommands that move HEAD are
  mutating, and only `log`/`view`/`terms` stay read-only.
- `git grep --open-files-in-pager=<cmd>` (and the glued `-O<cmd>` spelling)
  runs that program on every matching file, which made a search that
  classified safe-read arbitrary code execution. `grep` now has a handler:
  the pager options are opaque and privileged, option values are no longer
  mistaken for pathspecs, and the operands that really are files — everything
  after a `--` separator, and with `--no-index` the operands after the
  pattern — are declared reads, so `git grep --no-index -e . -- .env` meets
  the secret-file deny instead of printing the file.
- `git blame --contents <path> HEAD -- <tracked>` prints every line of
  `<path>`, wherever it is: `git blame --contents ~/.ssh/id_rsa …` was
  allowed and returned the key. `blame` and its older name `annotate` now
  declare the value of `--contents` and `-S` as reads, so the secret-file
  hard deny and the outside-the-workspace checks see the path the command
  actually opens.
- The commands that take git's diff options write the file named by
  `--output=<file>`: `git show --output=/etc/x HEAD` and `git diff
  --output=/tmp/z` both classified `allow (safe-read)` while writing a file
  the policy never saw — anywhere at all without a sandbox, and in plan mode
  a "read-only" command that writes. `diff`, `show`, `log`, `whatchanged`,
  `range-diff` and the `diff-tree`/`diff-index`/`diff-files` plumbing now
  declare that value as a write, the way the reader specs already treat
  `sort -o`.
- `git -C <dir>`, `--git-dir=<dir>` and `--work-tree=<dir>` were skipped
  without looking at the value. A repository outside the workspace brings its
  own configuration with it — aliases, a hooks path, filter drivers — and
  every path the rest of the command names is resolved against it, so the
  analysis described a different tree from the one that ran. Such a command
  is now opaque and can never ride an allow rule, and because the option
  taints the subcommand instead of replacing it, a hard deny the subcommand
  raises (`git -C … push --force origin main`) still stands.
- `git config --file <path>` is an ordinary file reader and writer: it wrote
  `~/.bashrc` as "git config write" and read a credential file as "git config
  read", with the path invisible to both. The value of `--file`/`-f` is now
  declared, and it is no longer counted as a config key when deciding whether
  the command reads or writes.
- The same audit covered the rest of the git table: `difftool`/`mergetool`
  `--extcmd`/`-x` run a command of the caller's choosing for every changed
  file (now opaque and privileged) and `--tool` runs a configured one (now
  opaque); `git archive -o`, `git format-patch -o` and `git bundle create`
  declare the file or directory they write, and `git archive --remote`
  reports that it needs the network. `git for-each-ref --format` was checked
  and does not execute anything: its `--shell`/`--python`/`--perl` switches
  only quote the output.
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
