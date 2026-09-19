package shellclass

import (
	"regexp"
	"slices"
	"strings"
)

func registerInfra() {
	register(handleDocker, "docker", "podman", "nerdctl")
	register(handleCompose, "docker-compose", "podman-compose")
	register(handleKubectl, "kubectl", "oc", "k")
	register(handleHelm, "helm")
	register(handleTerraform, "terraform", "tofu", "terragrunt")
	register(handlePulumi, "pulumi")
	register(handleDB, "psql", "mysql", "mariadb", "mongosh", "mongo", "redis-cli", "clickhouse-client", "usql", "pgcli", "mycli", "sqlite3", "sqlite", "duckdb", "litecli")
	register(handleCloud, "aws", "gcloud", "gsutil", "az", "doctl", "flyctl", "fly", "heroku", "vercel", "netlify", "wrangler", "oci", "ibmcloud", "linode-cli", "scw", "cdk", "sst", "serverless", "sls", "ansible", "ansible-playbook", "railway", "render", "supabase", "firebase", "eb", "sam", "copilot")
}

// ---- docker / podman -----------------------------------------------------------

var (
	dockerSafe = map[string]bool{
		"ps": true, "images": true, "logs": true, "inspect": true, "version": true, "info": true, "stats": true, "top": true,
		"port": true, "history": true, "diff": true, "events": true, "context": true, "--version": true, "help": true, "": true,
	}
	dockerDestructive = map[string]bool{"rm": true, "rmi": true, "prune": true, "remove": true}
	dockerSubDestruct = map[string]bool{"system": true, "image": true, "container": true, "volume": true, "network": true, "builder": true, "buildx": true}
	dockerPrivileged  = []string{"--privileged", "--pid=host", "--userns=host", "--cap-add=ALL", "--cap-add=SYS_ADMIN", "--cap-add=SYS_PTRACE", "--security-opt=seccomp=unconfined", "--security-opt=apparmor=unconfined"}
	dockerSockets     = []string{"docker.sock", "podman.sock", "podman/podman.sock", "containerd.sock"}
)

// handleDocker: reads are safe, lifecycle commands need the daemon and
// usually the network, removals are destructive, and any flag that escapes
// the container (privileged, host pid, bind-mounting / or the daemon
// socket) is hard-denied.
func handleDocker(a *analyzer, name string, args []word) result {
	if hd := a.dockerEscape(name, args); hd != "" {
		return privilegeDeny(hd)
	}
	i := dockerGlobalOptions(args)
	if i >= len(args) {
		return safe(name)
	}
	sub := args[i].text
	rest := args[i+1:]
	if sub == "compose" {
		return handleCompose(a, name+" compose", rest)
	}
	return dockerVerb(name, sub, first(rest))
}

// dockerGlobalOptions returns the index of the subcommand after the client's
// global options.
func dockerGlobalOptions(args []word) int {
	i := 0
	for i < len(args) && isFlag(args[i].text) {
		switch args[i].text {
		case "-H", "--host", "--context", "-c", "--config", "-l", "--log-level", "--url", "--connection", "--root", "--runroot":
			i += 2
		default:
			i++
		}
	}
	return i
}

// dockerSubReads are the read-only verbs of the object subcommands
// (`docker image ls`, `docker volume inspect`…).
var dockerSubReads = map[string]bool{"ls": true, "list": true, "inspect": true, "df": true, "history": true, "logs": true, "ps": true, "top": true, "stats": true, "port": true, "diff": true, "events": true, "show": true}

// dockerVerb classifies `name sub next`.
func dockerVerb(name, sub, next string) result {
	switch {
	case dockerDestructive[sub], dockerSubDestruct[sub] && dockerDestructive[next], sub == "kill":
		return destructive(name + " " + sub + " " + next)
	case dockerSafe[sub], dockerSubDestruct[sub] && dockerSubReads[next]:
		return safe(name + " " + sub + " " + next)
	case sub == "login":
		return privilegeDenyNet(name + " login stores registry credentials")
	}
	return network(name + " " + sub)
}

// dockerEscape reports the flag that lets a container escape confinement.
func (a *analyzer) dockerEscape(name string, args []word) string {
	for i := range args {
		if flag := privilegedFlag(args, i); flag != "" {
			return name + " " + flag + " escapes the container"
		}
		if src := bindSource(args, i); src != "" {
			if why := a.mountEscape(src); why != "" {
				return name + " mounts " + why
			}
		}
	}
	return ""
}

// privilegedFlag returns the escape flag at args[i] ("--privileged",
// "--pid host"…) or "".
func privilegedFlag(args []word, i int) string {
	t := args[i].text
	for _, p := range dockerPrivileged {
		if t == p || strings.HasPrefix(t, p+"=") {
			return t
		}
	}
	if (t == "--pid" || t == "--userns" || t == "--cap-add" || t == "--security-opt") && i+1 < len(args) {
		v := args[i+1].text
		if v == "host" || v == "ALL" || v == "SYS_ADMIN" || v == "SYS_PTRACE" || strings.Contains(v, "unconfined") {
			return t + " " + v
		}
	}
	return ""
}

// bindSource extracts the host path of a -v/--volume/--mount argument at i.
func bindSource(args []word, i int) string {
	t := args[i].text
	var spec string
	switch {
	case (t == "-v" || t == "--volume") && i+1 < len(args):
		spec = args[i+1].text
	case strings.HasPrefix(t, "--volume="):
		spec = strings.TrimPrefix(t, "--volume=")
	case t == "--mount" && i+1 < len(args):
		spec = args[i+1].text
	case strings.HasPrefix(t, "--mount="):
		spec = strings.TrimPrefix(t, "--mount=")
	default:
		return ""
	}
	if strings.Contains(spec, "source=") || strings.Contains(spec, "src=") {
		for part := range strings.SplitSeq(spec, ",") {
			if v, ok := strings.CutPrefix(part, "source="); ok {
				return v
			}
			if v, ok := strings.CutPrefix(part, "src="); ok {
				return v
			}
		}
		return ""
	}
	src, _, _ := strings.Cut(spec, ":")
	return src
}

// mountEscape says why bind-mounting src defeats the sandbox.
func (a *analyzer) mountEscape(src string) string {
	for _, sock := range dockerSockets {
		if strings.HasSuffix(src, sock) {
			return "the container daemon socket"
		}
	}
	if !strings.HasPrefix(src, "/") && !strings.HasPrefix(src, "~") && !strings.HasPrefix(src, "$") {
		return "" // named volume
	}
	abs, _, err := a.ws.Resolve(src)
	if err != nil {
		return ""
	}
	switch why := a.catastrophicTarget(abs); {
	case why != "" && why != "a parent of the workspace":
		return why
	case slices.Contains(systemDirs, abs):
		return "system directory " + abs
	}
	return ""
}

// handleCompose: `down -v` and `rm` are destructive, reads are safe, the
// rest needs the daemon and typically the network.
func handleCompose(a *analyzer, name string, args []word) result {
	sub := first(args)
	switch sub {
	case "down":
		if hasFlag(args, "-v", "--volumes", "--rmi") {
			return destructive(name + " down -v removes volumes")
		}
		return mutating(name + " down")
	case "rm":
		return destructive(name + " rm")
	case "ps", "logs", "config", "images", "ls", "top", "version", "events", "port", "":
		return safe(name + " " + sub)
	}
	return network(name + " " + sub)
}

// ---- kubernetes -----------------------------------------------------------------

var (
	kubeReads = map[string]bool{
		"get": true, "describe": true, "logs": true, "top": true, "explain": true, "version": true, "cluster-info": true,
		"api-resources": true, "api-versions": true, "auth": true, "diff": true, "events": true, "wait": true, "": true,
	}
	kubeDestructive = map[string]bool{"delete": true, "drain": true}
)

// handleKubectl: even reads reach the cluster, so nothing here is SafeRead;
// deletions and node drains are destructive.
func handleKubectl(a *analyzer, name string, args []word) result {
	i := 0
	for i < len(args) && isFlag(args[i].text) {
		switch args[i].text {
		case "-n", "--namespace", "--context", "--kubeconfig", "-s", "--server", "--cluster", "--user", "--token":
			i += 2
		default:
			i++
		}
	}
	if i >= len(args) {
		return safe(name)
	}
	sub := args[i].text
	next := first(args[i+1:])
	switch {
	case kubeDestructive[sub], sub == "replace" && hasFlag(args, "--force"):
		return destructive(name + " " + sub)
	case sub == "config":
		if next == "view" || next == "current-context" || next == "get-contexts" || next == "get-clusters" || next == "get-users" || next == "" {
			return safe(name + " config read")
		}
		return mutating(name + " config " + next + " edits kubeconfig")
	case kubeReads[sub]:
		r := network(name + " " + sub + " reads cluster state")
		return r
	case sub == "exec", sub == "debug":
		return opaque(name + " " + sub + " runs commands in the cluster")
	}
	return network(name + " " + sub + " changes cluster state")
}

func handleHelm(a *analyzer, name string, args []word) result {
	sub := first(args)
	nf := texts(nonFlags(args))
	switch sub {
	case "template", "lint", "version", "env", "show", "verify", "create", "completion", "":
		return safe("helm " + sub)
	case "uninstall", "delete", "del", "rollback":
		return destructive("helm " + sub)
	case "repo":
		if len(nf) > 1 && (nf[1] == "list" || nf[1] == "ls") {
			return safe("helm repo list")
		}
		return network("helm repo " + strings.Join(nf[1:], " "))
	case "registry":
		return privilegeDeny("helm registry login stores credentials")
	}
	return network("helm " + sub)
}

// ---- infrastructure as code ------------------------------------------------------

func handleTerraform(a *analyzer, name string, args []word) result {
	sub := first(args)
	nf := texts(nonFlags(args))
	switch sub {
	case "fmt", "validate", "show", "output", "version", "graph", "console", "providers", "test", "metadata", "":
		return safe(name + " " + sub)
	case "init", "plan", "refresh", "get":
		return network(name + " " + sub)
	case "apply", "destroy", "import", "taint", "untaint", "force-unlock", "run-all":
		return destructive(name + " " + sub + " changes real infrastructure")
	case "state":
		if len(nf) > 1 && (nf[1] == "list" || nf[1] == "show" || nf[1] == "pull") {
			return network(name + " state " + nf[1])
		}
		return destructive(name + " state " + strings.Join(nf[1:], " ") + " rewrites state")
	case "workspace":
		if len(nf) > 1 && nf[1] == "delete" {
			return destructive(name + " workspace delete")
		}
		if len(nf) > 1 && (nf[1] == "list" || nf[1] == "show") {
			return safe(name + " workspace " + nf[1])
		}
		return mutating(name + " workspace")
	case "login":
		return privilegeDeny(name + " login stores credentials")
	}
	return network(name + " " + sub)
}

func handlePulumi(a *analyzer, name string, args []word) result {
	sub := first(args)
	nf := texts(nonFlags(args))
	switch sub {
	case "version", "about", "whoami", "config", "":
		return safe("pulumi " + sub)
	case "preview", "pre", "logs", "history", "env", "org", "policy", "plugin", "new":
		return network("pulumi " + sub)
	case "up", "update", "destroy", "refresh", "cancel", "import":
		return destructive("pulumi " + sub + " changes real infrastructure")
	case "state", "stack":
		if len(nf) > 1 && (nf[1] == "rm" || nf[1] == "delete" || nf[1] == "unprotect" || nf[1] == "rename") {
			return destructive("pulumi " + sub + " " + nf[1])
		}
		return network("pulumi " + sub)
	case "login", "logout":
		return privilegeDeny("pulumi login stores credentials")
	}
	return network("pulumi " + sub)
}

// ---- databases --------------------------------------------------------------------

var (
	sqlDestructive = regexp.MustCompile(`(?i)\b(DROP\s+(TABLE|DATABASE|SCHEMA|INDEX|VIEW|USER|ROLE|COLLECTION)|TRUNCATE\b|FLUSHALL\b|FLUSHDB\b|ALTER\s+TABLE\s+\S+\s+DROP\b|dropDatabase|\.drop\(|deleteMany\(\s*\{\s*\}|SHUTDOWN\b|CONFIG\s+SET)`)
	sqlDeleteFrom  = regexp.MustCompile(`(?i)\bDELETE\s+FROM\b`)
	sqlWhere       = regexp.MustCompile(`(?i)\bWHERE\b`)
	localDBs       = []string{"sqlite3", "sqlite", "duckdb", "litecli"}
)

// handleDB scans inline statements for destructive SQL (DROP, TRUNCATE,
// DELETE without WHERE, FLUSHALL…). Remote clients are Network; local
// file databases are Mutating.
func handleDB(a *analyzer, name string, args []word) result {
	r := network(name)
	if slices.Contains(localDBs, name) {
		r = mutating(name)
		if nf := nonFlags(args); len(nf) > 0 {
			a.writeFiles(&r, nf[:1], false)
		}
	}
	joined := strings.Join(texts(args), " ")
	if destructiveSQL(joined) {
		r.raise(Destructive)
		r.reason = joinReason(r.reason, "destructive SQL statement")
	}
	if name == "redis-cli" {
		for _, t := range texts(nonFlags(args)) {
			if strings.EqualFold(t, "DEL") || strings.EqualFold(t, "FLUSHALL") || strings.EqualFold(t, "FLUSHDB") {
				r.raise(Destructive)
				r.reason = joinReason(r.reason, "redis "+strings.ToUpper(t))
			}
		}
	}
	return r
}

// destructiveSQL reports DROP/TRUNCATE/FLUSH or a DELETE FROM with no WHERE
// in the same statement.
func destructiveSQL(s string) bool {
	if sqlDestructive.MatchString(s) {
		return true
	}
	for stmt := range strings.SplitSeq(s, ";") {
		if sqlDeleteFrom.MatchString(stmt) && !sqlWhere.MatchString(stmt) {
			return true
		}
	}
	return false
}

// ---- cloud CLIs -------------------------------------------------------------------

var (
	cloudDestructiveVerbs = []string{"delete", "destroy", "terminate", "terminate-instances", "purge", "rm", "rb", "remove", "deregister", "revoke", "delete-stack", "delete-bucket", "delete-table", "delete-function", "teardown", "down", "kill", "nuke"}
	cloudSecretPhrases    = []string{
		"secretsmanager get-secret-value", "ssm get-parameter --with-decryption", "ssm get-parameters --with-decryption", "iam create-access-key",
		"configure export-credentials", "secrets versions access", "auth print-access-token", "auth print-identity-token", "auth application-default print-access-token",
		"keyvault secret show", "keyvault secret download", "account get-access-token", "auth token", "secrets get", "secrets list --show-values", "kms decrypt",
	}
	cloudLoginVerbs = []string{"configure", "login", "auth", "sso", "logout", "init"}
)

// handleCloud: cloud CLIs always reach the network. Deleting verbs are
// destructive; commands that print credential material are hard-denied
// (the output would land in the transcript); login flows are privileged.
func handleCloud(a *analyzer, name string, args []word) result {
	nf := texts(nonFlags(args))
	joined := " " + strings.Join(texts(args), " ") + " "
	for _, phrase := range cloudSecretPhrases {
		if strings.Contains(joined, " "+phrase+" ") || strings.HasSuffix(joined, " "+phrase+" ") {
			return privilegeDenyNet(name + " " + phrase + " prints credentials")
		}
	}
	for _, t := range nf {
		if slices.Contains(cloudDestructiveVerbs, t) || strings.HasPrefix(t, "delete-") || strings.HasPrefix(t, "terminate-") {
			return destructive(name + " " + t)
		}
	}
	if hasFlag(args, "--delete") {
		return destructive(name + " --delete")
	}
	if len(nf) > 0 && slices.Contains(cloudLoginVerbs, nf[0]) {
		return result{class: Privilege, reason: name + " " + nf[0] + " changes stored credentials"}
	}
	return network(name + " " + first(args))
}
