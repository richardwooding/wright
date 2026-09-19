package shellclass

import (
	"path/filepath"
	"slices"
	"strings"
)

func registerFiles() {
	register(handleRm, "rm", "rmdir", "shred", "unlink", "trash")
	register(handleCopy, "mv", "cp", "install")
	register(handleRsync, "rsync")
	register(handleTar, "tar")
	register(handleArchive, "zip", "unzip", "gzip", "gunzip", "bzip2", "bunzip2", "xz", "unxz", "zstd", "unzstd", "7z", "7za")
	register(handleCreate, "mkdir", "touch", "ln", "mkfifo", "mktemp")
	register(handleTruncate, "truncate")
	register(handleChmod, "chmod", "chown", "chgrp", "chattr", "setfacl")
	register(handleDD, "dd")
}

// pathWords resolves positional words to absolute paths (dynamic ones dropped).
func (a *analyzer) pathWords(ws []word) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		if w.dynamic {
			continue
		}
		if abs, _, err := a.ws.Resolve(w.text); err == nil {
			out = append(out, abs)
		}
	}
	return out
}

// catastrophicTarget names why deleting abs would be irrecoverable: the
// filesystem root, the home directory, a workspace root or any ancestor of
// one, or a protected location.
func (a *analyzer) catastrophicTarget(abs string) string {
	switch {
	case abs == "/" || abs == filepath.VolumeName(abs)+string(filepath.Separator):
		return "the filesystem root"
	case abs == a.ws.Home():
		return "the home directory"
	case a.ws.IsProtected(abs):
		return "protected path " + a.display(abs)
	}
	for _, root := range a.ws.Roots() {
		if abs == root {
			return "the workspace root"
		}
		if strings.HasPrefix(root, abs+string(filepath.Separator)) {
			return "a parent of the workspace"
		}
	}
	return ""
}

// handleRm: deletion is destructive. Catastrophic targets are hard-denied. A
// single non-recursive removal of an untracked workspace file is merely
// mutating — the cost of that mistake is one file the user never committed.
func handleRm(a *analyzer, name string, args []word) result {
	targets := nonFlags(args)
	recursive := hasShort(args, 'r') || hasShort(args, 'R') || hasFlag(args, "--recursive") || name == "rmdir"
	dynamic := false
	for _, t := range targets {
		dynamic = dynamic || t.dynamic
	}
	r := destructive(name + " removes files")
	abs := a.pathWords(targets)
	r.writes = abs
	for _, p := range abs {
		if why := a.catastrophicTarget(p); why != "" {
			return result{class: Destructive, reason: name + " targets " + why, hardDeny: name + " targets " + why, writes: abs}
		}
	}
	if len(targets) == 1 && !recursive && !dynamic && len(abs) == 1 {
		if _, inside, err := a.ws.Resolve(targets[0].text); err == nil && inside && !a.ws.IsTracked(abs[0]) && !a.ws.IsSecretFile(abs[0]) {
			return result{class: MutatingWorkspace, reason: name + " of untracked file " + a.display(abs[0]), writes: abs}
		}
	}
	return r
}

// handleCopy covers mv/cp/install: sources are reads (a secret source is
// hard-denied — copying .env elsewhere is how it gets read later), the
// destination is a write that may overwrite a tracked file.
func handleCopy(a *analyzer, name string, args []word) result {
	r := mutating(name)
	nf := nonFlags(args)
	dest, hasT := flagValue(args, "-t", "--target-directory")
	if !hasT && len(nf) >= 2 {
		dest, nf = nf[len(nf)-1], nf[:len(nf)-1]
	}
	for _, src := range nf {
		if src.dynamic {
			continue
		}
		if abs, _, err := a.ws.Resolve(src.text); err == nil {
			r.reads = append(r.reads, abs)
			if a.ws.IsSecretFile(abs) {
				return result{class: Destructive, reason: name + " of secret file " + a.display(abs), hardDeny: name + " of secret file " + a.display(abs), reads: r.reads}
			}
			if name == "mv" {
				if why := a.catastrophicTarget(abs); why != "" {
					return result{class: Destructive, reason: "mv of " + why, hardDeny: "mv of " + why}
				}
			}
		}
	}
	if dest.text != "" {
		a.writeFiles(&r, []word{dest}, true)
	}
	return r
}

// handleRsync: --delete makes it destructive; a remote endpoint makes it
// network; otherwise it writes the destination.
func handleRsync(a *analyzer, name string, args []word) result {
	nf := nonFlags(args)
	remote := false
	for _, w := range nf {
		if strings.Contains(w.text, "::") || strings.HasPrefix(w.text, "rsync://") || (strings.Contains(w.text, ":") && !strings.Contains(strings.SplitN(w.text, ":", 2)[0], "/")) {
			remote = true
		}
	}
	r := mutating("rsync")
	if remote {
		r = network("rsync to/from a remote host")
	}
	for _, w := range args {
		if strings.HasPrefix(w.text, "--delete") || w.text == "--remove-source-files" {
			r.raise(Destructive)
			r.reason = joinReason(r.reason, "rsync "+w.text+" removes files")
		}
	}
	if !remote && len(nf) >= 2 {
		a.writeFiles(&r, nf[len(nf)-1:], false)
	}
	return r
}

// handleTar: listing is safe; create/extract write (extraction into a
// protected directory is caught through -C).
func handleTar(a *analyzer, name string, args []word) result {
	if len(args) == 0 {
		return safe("tar")
	}
	mode := args[0].text
	cluster := strings.TrimPrefix(mode, "-")
	list := strings.ContainsRune(cluster, 't') && !strings.HasPrefix(mode, "--") || hasFlag(args, "--list")
	extract := strings.ContainsRune(cluster, 'x') && !strings.HasPrefix(mode, "--") || hasFlag(args, "--extract", "--get")
	switch {
	case list:
		return safe("tar list")
	case extract:
		r := mutating("tar extracts files")
		if dir, ok := flagValue(args, "-C", "--directory"); ok {
			a.writeFiles(&r, []word{dir}, false)
		}
		return r
	}
	r := mutating("tar writes an archive")
	if f, ok := flagValue(args, "-f", "--file"); ok {
		a.writeFiles(&r, []word{f}, true)
	} else if strings.ContainsRune(cluster, 'f') && len(args) > 1 {
		a.writeFiles(&r, args[1:2], true)
	}
	return r
}

// handleArchive: listing/testing/streaming to stdout is safe; anything else
// replaces or creates files.
func handleArchive(a *analyzer, name string, args []word) result {
	if hasFlag(args, "-l", "-t", "-c", "--stdout", "--to-stdout", "--list", "--test", "-k", "--keep", "-Z") || hasShort(args, 'l') || hasShort(args, 't') || hasShort(args, 'c') || hasShort(args, 'k') {
		r := safe(name + " read")
		a.readFiles(&r, nonFlags(args))
		return r
	}
	r := mutating(name + " replaces files")
	a.writeFiles(&r, nonFlags(args), false)
	return r
}

// handleCreate: mkdir/touch/ln write their targets (ln: the last one).
func handleCreate(a *analyzer, name string, args []word) result {
	r := mutating(name)
	nf := nonFlags(args)
	if name == "ln" && len(nf) >= 2 {
		nf = nf[len(nf)-1:]
	}
	a.writeFiles(&r, nf, false)
	return r
}

// handleTruncate treats `truncate -s 0 f` like `> f`.
func handleTruncate(a *analyzer, name string, args []word) result {
	r := mutating("truncate")
	a.writeFiles(&r, nonFlags(args), true)
	return r
}

// systemDirs are the roots a recursive chmod/chown must never touch.
var systemDirs = []string{"/", "/usr", "/etc", "/bin", "/sbin", "/lib", "/lib64", "/var", "/opt", "/boot", "/sys", "/proc", "/dev", "/home", "/root", "/Users", "/System", "/Library"}

// handleChmod: permission changes write their targets; recursive changes on
// system directories or $HOME are hard-denied.
func handleChmod(a *analyzer, name string, args []word) result {
	nf := nonFlags(args)
	if len(nf) > 0 && !hasFlag(args, "--reference") {
		nf = nf[1:] // mode / owner
	}
	recursive := hasShort(args, 'R') || hasFlag(args, "--recursive")
	r := mutating(name)
	for _, abs := range a.pathWords(nf) {
		if recursive && (slices.Contains(systemDirs, abs) || abs == a.ws.Home()) {
			return privilegeDeny(name + " -R on " + abs)
		}
	}
	a.writeFiles(&r, nf, false)
	if name == "chown" || name == "chgrp" {
		r.raise(MutatingWorkspace)
	}
	return r
}

// handleDD: writing a block device is hard-denied; writing a file follows the
// redirect rules; with no of= it is a reader.
func handleDD(a *analyzer, name string, args []word) result {
	var in, out *word
	for i := range args {
		switch {
		case strings.HasPrefix(args[i].text, "if="):
			w := word{text: strings.TrimPrefix(args[i].text, "if="), dynamic: args[i].dynamic}
			in = &w
		case strings.HasPrefix(args[i].text, "of="):
			w := word{text: strings.TrimPrefix(args[i].text, "of="), dynamic: args[i].dynamic}
			out = &w
		}
	}
	r := safe("dd")
	if in != nil {
		a.readFiles(&r, []word{*in})
	}
	if out == nil {
		return r
	}
	if out.dynamic {
		return opaque("dd with a dynamic of=")
	}
	if strings.HasPrefix(out.text, "/dev/") && !isDevSink(out.text) {
		return privilegeDeny("dd writes to device " + out.text)
	}
	r.raise(MutatingWorkspace)
	a.writeFiles(&r, []word{*out}, true)
	return r
}

// ---- system administration ---------------------------------------------------

func registerSystem() {
	register(func(a *analyzer, name string, args []word) result {
		return privilegeDeny("system administration command " + name)
	},
		"mkfs", "mkfs.ext4", "mkfs.xfs", "mkfs.btrfs", "mkfs.vfat", "mkfs.fat", "fdisk", "sfdisk", "parted", "gdisk", "wipefs", "mkswap",
		"shutdown", "reboot", "halt", "poweroff", "init", "telinit", "mount", "umount", "iptables", "ip6tables", "nft",
		"modprobe", "insmod", "rmmod", "chroot", "setcap", "useradd", "userdel", "usermod", "groupadd", "groupdel", "passwd",
		"chpasswd", "visudo", "losetup", "swapon", "swapoff", "dmsetup", "cryptsetup", "nsenter", "unshare", "at", "atrm",
		"tcpdump", "chsh", "pivot_root", "reboot", "ufw", "firewall-cmd", "sysctl -w",
	)
	register(handleSystemctl, "systemctl", "service", "rc-service")
	register(handleLaunchctl, "launchctl")
	register(handleCrontab, "crontab")
	register(handleKill, "kill", "killall", "pkill")
	register(handleIP, "ip", "ifconfig", "route")
	register(handleSysctl, "sysctl")
	register(handlePkgManager, "apt", "apt-get", "dnf", "yum", "pacman", "apk", "zypper", "rpm", "snap", "flatpak", "port", "emerge", "nix-env", "rpm-ostree")
	register(handleDefaults, "defaults")
	register(func(a *analyzer, name string, args []word) result {
		return privilegeDeny(name + " reads a password store")
	},
		"pass", "gopass", "op", "bw", "vault", "security", "keyctl", "secret-tool", "lpass", "doppler")
	register(handleGpg, "gpg", "gpg2")
	register(handleSSHKeygen, "ssh-keygen", "ssh-add")
}

func handleSystemctl(a *analyzer, name string, args []word) result {
	switch first(args) {
	case "status", "list-units", "list-unit-files", "list-timers", "list-sockets", "list-jobs", "list-dependencies", "show",
		"cat", "is-active", "is-enabled", "is-failed", "is-system-running", "show-environment", "get-default", "--version", "help", "":
		return safe(name + " read")
	}
	return privilegeDeny(name + " " + first(args) + " changes system services")
}

func handleLaunchctl(a *analyzer, name string, args []word) result {
	switch first(args) {
	case "list", "print", "print-disabled", "getenv", "version", "help", "blame", "":
		return safe("launchctl read")
	}
	return privilegeDeny("launchctl " + first(args) + " changes launch services")
}

func handleCrontab(a *analyzer, name string, args []word) result {
	if hasFlag(args, "-l") && !hasFlag(args, "-e", "-r") && len(nonFlags(args)) == 0 {
		return safe("crontab -l")
	}
	return privilegeDeny("crontab installs scheduled commands")
}

// handleKill: `kill -9 -1` (every process the user owns) is hard-denied.
func handleKill(a *analyzer, name string, args []word) result {
	for _, t := range texts(args) {
		if t == "-1" && name == "kill" {
			return privilegeDeny("kill -1 signals every process")
		}
	}
	if name != "kill" && len(nonFlags(args)) == 0 {
		return privilegeDeny(name + " without a pattern")
	}
	return mutating(name + " signals processes")
}

func handleIP(a *analyzer, name string, args []word) result {
	for _, t := range texts(nonFlags(args)) {
		switch t {
		case "add", "del", "delete", "set", "flush", "change", "replace", "up", "down":
			return privilegeDeny(name + " changes network configuration")
		}
	}
	return safe(name + " read")
}

func handleSysctl(a *analyzer, name string, args []word) result {
	if hasFlag(args, "-w", "--write", "-p", "--load", "--system") {
		return privilegeDeny("sysctl writes kernel parameters")
	}
	for _, t := range texts(nonFlags(args)) {
		if strings.Contains(t, "=") {
			return privilegeDeny("sysctl writes kernel parameters")
		}
	}
	return safe("sysctl read")
}

// handlePkgManager: system package managers need root; queries are safe.
func handlePkgManager(a *analyzer, name string, args []word) result {
	switch first(args) {
	case "list", "search", "show", "info", "policy", "depends", "rdepends", "provides", "-Q", "-Qi", "-Ss", "-Si", "-q", "-qa", "-qi", "-ql", "status", "--version", "version", "":
		if name == "rpm" && (hasShort(args, 'i') || hasShort(args, 'e') || hasShort(args, 'U')) {
			return privilegeDeny("rpm installs or erases packages")
		}
		return safe(name + " query")
	}
	return result{class: Privilege, reason: name + " " + first(args) + " needs root"}
}

func handleDefaults(a *analyzer, name string, args []word) result {
	if first(args) == "read" || first(args) == "domains" || first(args) == "find" {
		return safe("defaults read")
	}
	return result{class: Privilege, reason: "defaults write changes macOS preferences"}
}

func handleGpg(a *analyzer, name string, args []word) result {
	switch {
	case hasFlag(args, "--export-secret-keys", "--export-secret-subkeys", "--export-ssh-key"):
		return privilegeDeny("gpg exports private keys")
	case hasFlag(args, "--list-keys", "-k", "-K", "--list-secret-keys", "--version", "--fingerprint", "--verify", "--list-packets"):
		return safe("gpg read")
	case hasFlag(args, "--decrypt", "-d"):
		r := mutating("gpg decrypt")
		a.readFiles(&r, nonFlags(args))
		return r
	}
	return mutating("gpg")
}

func handleSSHKeygen(a *analyzer, name string, args []word) result {
	r := mutating(name)
	if f, ok := flagValue(args, "-f"); ok {
		if name == "ssh-add" || hasFlag(args, "-y", "-l", "-e", "-p") {
			a.readFiles(&r, []word{f})
		} else {
			a.writeFiles(&r, []word{f}, false)
		}
	}
	if name == "ssh-add" {
		a.readFiles(&r, nonFlags(args))
	}
	return r
}

// ---- interpreters ---------------------------------------------------------------

func registerInterpreters() {
	register(handleInterpreter, interpreters...)
	register(handleJava, "java")
	register(func(a *analyzer, name string, args []word) result { return opaque(name + " launches an application") }, "open", "xdg-open", "start", "screen", "tmux", "vim", "vi", "nvim", "nano", "emacs", "code", "subl")
}

// handleInterpreter: inline code is opaque; a workspace script is mutating;
// modules run in place; a REPL/stdin is opaque.
func handleInterpreter(a *analyzer, name string, args []word) result {
	if len(args) == 0 {
		return opaque(name + " REPL reads code from stdin")
	}
	if hasFlag(args, "-c", "-e", "--eval", "-p", "--print", "-r", "--exec", "-C", "-Command") && name != "perl" || (name == "perl" && (hasFlag(args, "-e", "-E") || hasShort(args, 'e'))) {
		return opaque(name + " runs inline code")
	}
	if hasFlag(args, "-m", "--module") {
		return mutating(name + " runs a module")
	}
	nf := nonFlags(args)
	switch {
	case len(nf) == 0 && hasFlag(args, "--version", "-V", "-v", "--help", "-h"):
		return safe(name + " version")
	case len(nf) == 0:
		return opaque(name + " reads code from stdin")
	}
	sub := nf[0].text
	switch name {
	case "deno":
		return denoVerb(sub)
	case "node":
		if sub == "--test" {
			return mutating("node --test")
		}
	}
	if strings.HasPrefix(sub, "http://") || strings.HasPrefix(sub, "https://") {
		return network(name + " runs remote code")
	}
	return a.scriptFile(name, nf[0])
}

func denoVerb(sub string) result {
	switch sub {
	case "install", "add", "cache", "upgrade", "publish", "remove":
		return network("deno " + sub)
	case "run", "test", "task", "fmt", "lint", "check", "compile", "bundle", "bench", "doc", "eval", "repl", "serve":
		if sub == "eval" || sub == "repl" {
			return opaque("deno " + sub + " runs inline code")
		}
		return mutating("deno " + sub)
	}
	return mutating("deno " + sub)
}

func handleJava(a *analyzer, name string, args []word) result {
	if jar, ok := flagValue(args, "-jar"); ok {
		return a.scriptFile("java", jar)
	}
	if hasFlag(args, "-version", "--version", "-help", "--help") {
		return safe("java version")
	}
	return mutating("java runs a class")
}
