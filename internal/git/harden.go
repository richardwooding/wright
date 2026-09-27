package git

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
)

// neutralised are configuration keys that name a program for git to run. A
// repository carries its own .git/config, so cloning a hostile one and
// running an ordinary `git diff` or `git status` is enough to execute it —
// and those commands are allowed by default because they are how an agent
// reads a repository. Nothing static can see this: the command line is
// innocent. Overriding the keys in the environment is what closes it.
//
// Measured against git 2.55, per subcommand: core.fsmonitor runs on Status,
// Diff and IsTracked; diff.external and the attribute-driven keys below run
// on Diff. Only `rev-parse --show-toplevel` runs nothing — it reads no index,
// which is what fsmonitor hangs off. Until this block was applied to
// host-side calls, all three of the others ran whatever the repository named,
// and Status is polled every five seconds while the session is idle.
//
// Every key here must tolerate an *empty value*, and that is a sharper
// constraint than it looks: git has no "unset" through this mechanism, so a
// blanked key is a key set to the empty string. For most of these that means
// "none" — credential.helper documents an empty value as resetting the helper
// list, an empty proxy means no proxy, an empty filter passes bytes through —
// but for a key naming a program to *run*, git dutifully tries to run "" and
// dies with `cannot run : No such file or directory`.
//
// Two keys were in this list and are measurably not safe here (git 2.55):
//
//   - diff.external — blanking it breaks `git diff` in *every* repository,
//     hostile or not, with "fatal: external diff died". Diff below passes
//     --no-ext-diff instead, which is git's own way to ignore it and needs no
//     value. A sandboxed git the agent runs itself keeps the repository's
//     setting: containment there is the sandbox's job, not this block's.
//   - core.sshCommand — blanking it breaks every ssh remote operation,
//     `git push` included, with the same "cannot run" error. wright issues no
//     remote operation on the host, so there is nothing here to close.
//
// Keys deliberately never added: core.hooksPath, gpg.program and
// uploadpack.packObjectsHook name programs too, but none of them fires on a
// read-only command, .git/hooks is already in the sandbox's protected set,
// and blanking core.hooksPath would disable the user's own pre-commit hooks —
// a worse trade than the one it closes. The attribute-driven family below is
// the one that does fire and that a fixed list cannot name.
var neutralised = []string{
	"core.fsmonitor", // runs on git status
	"credential.helper",
	"sequence.editor",
	"core.editor",
	"core.askpass", // runs when no helper produced a credential
	// The proxy keys are not programs: they decide who *receives* an
	// authenticated request. They were harmless while no credential could be
	// produced inside the sandbox; once one can, a cloned repository's own
	// http.proxy is a way to be handed the request that carries it. Empty
	// means "no proxy", so blanking them changes nothing for anyone who is
	// not behind one — and someone who is cannot reach the network from the
	// sandbox without --allow-network anyway.
	"http.proxy",
	"core.gitproxy",
}

// credentialHelperKey is credential.helper's position in neutralised, found
// rather than written down: reordering the list above must not silently
// blank a different key than the one a caller meant to replace. The static
// keys are always emitted first, so a repository's own keys appended after
// them cannot move it either.
func credentialHelperKey() int { return slices.Index(neutralised, "credential.helper") }

// attributeDriven reports whether key is one of the config keys whose middle
// segment is a *driver name the repository chose*, selected per path from the
// repository's own .gitattributes: filter.<driver>.clean/smudge/process and
// diff.<driver>.textconv/command.
//
// These are the second half of the problem and the reason neutralised alone
// is not enough. The list above can be written down because git fixes those
// names; these cannot, because the attacker picks them. Measured against git
// 2.55: filter.<d>.clean runs twice on a plain `git status` and again on
// `git diff`, and diff.<d>.textconv runs on `git diff` — and `git diff
// --no-ext-diff --no-textconv`, which needs no names, still leaves the filter
// half running. Enumerating the repository's own keys and blanking each by
// name is what closes it; see repoNamedKeys.
func attributeDriven(key string) bool {
	if strings.Count(key, ".") < 2 {
		return false // no driver segment; not one of these
	}
	switch {
	case strings.HasPrefix(key, "filter."):
		return hasAnySuffix(key, ".clean", ".smudge", ".process")
	case strings.HasPrefix(key, "diff."):
		return hasAnySuffix(key, ".textconv", ".command")
	}
	return false
}

func hasAnySuffix(s string, suffixes ...string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}

// repoNamedKeys lists the attribute-driven keys that dir's own configuration
// actually sets, so each can be blanked by its exact name.
//
// Reading the configuration is itself safe: `git config --list` resolves
// include.path but runs no driver, no filter and no hook, which is what makes
// this two-step approach possible at all. It runs with the static overrides
// already applied so that the one command needed to discover the rest is
// itself covered.
func repoNamedKeys(ctx context.Context, dir string) []string {
	out, err := runWith(ctx, dir, environWith(overrides(neutralised)), "config", "--local", "--list", "--name-only")
	if err != nil {
		return nil // not a repository, or no git: nothing to blank
	}
	var keys []string
	for line := range strings.SplitSeq(out, "\n") {
		key := strings.TrimSpace(line)
		if attributeDriven(key) && !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	return keys
}

// overrides renders keys as git's GIT_CONFIG_COUNT/KEY/VALUE block, each key
// set to empty. Entries injected this way take precedence over every config
// file, including the repository's own.
//
// The static keys always come first so credentialHelperKey stays a valid
// index into the block after the repository's own keys are appended.
func overrides(keys []string) map[string]string {
	env := map[string]string{"GIT_CONFIG_COUNT": strconv.Itoa(len(keys))}
	for i, key := range keys {
		env["GIT_CONFIG_KEY_"+strconv.Itoa(i)] = key
		env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)] = ""
	}
	return env
}

// environWith returns this process's environment with over applied on top.
// exec.Cmd keeps the last of a repeated name, so the overrides win.
func environWith(over map[string]string) []string {
	env := os.Environ()
	for key, value := range over {
		env = append(env, key+"="+value)
	}
	return env
}

// hostEnv is the environment for git run on the host: the user's own global
// config stays readable — WhoAmI depends on it, and it is theirs, not the
// repository's — with every key a repository could use to name a program
// blanked on top.
func hostEnv(ctx context.Context, dir string) []string {
	keys := append(slices.Clone(neutralised), repoNamedKeys(ctx, dir)...)
	return environWith(overrides(keys))
}

// SandboxEnv returns the environment entries wright adds to a sandboxed git:
// the commit identity resolved on the host, empty stand-ins for the config
// files the sandbox hides, and the same neutralising overrides hostEnv
// applies — including the ones named after this repository's own drivers.
func SandboxEnv(ctx context.Context, dir string) map[string]string {
	keys := append(slices.Clone(neutralised), repoNamedKeys(ctx, dir)...)
	env := overrides(keys)
	// The global and system files are unreadable inside the sandbox;
	// pointing git at an empty one turns a warning (or a fatal error under
	// landlock) into ordinary "no global config".
	env["GIT_CONFIG_GLOBAL"] = os.DevNull
	env["GIT_CONFIG_SYSTEM"] = os.DevNull
	// With credential.helper blank, a push that needs a credential would
	// otherwise block on git's terminal prompt inside a sandbox where
	// nobody can answer it. Failing immediately is the honest outcome.
	env["GIT_TERMINAL_PROMPT"] = "0"
	// With no identity configured wright carries none: git keeps its own
	// behaviour rather than committing under a name wright invented.
	if id := WhoAmI(ctx, dir); id.Name != "" && id.Email != "" {
		env["GIT_AUTHOR_NAME"] = id.Name
		env["GIT_AUTHOR_EMAIL"] = id.Email
		env["GIT_COMMITTER_NAME"] = id.Name
		env["GIT_COMMITTER_EMAIL"] = id.Email
	}
	return env
}
