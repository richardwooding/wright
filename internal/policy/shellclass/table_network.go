package shellclass

import (
	"strings"
)

func registerNetwork() {
	register(handleFetch, fetchers...)
	register(handleSSH, "ssh", "scp", "sftp", "rsh", "mosh", "telnet", "ftp", "ftps", "lftp")
	register(func(a *analyzer, name string, args []word) result { return network(name) },
		"nc", "ncat", "netcat", "socat", "nmap", "ping", "ping6", "traceroute", "tracepath", "mtr", "dig", "nslookup", "host",
		"whois", "openssl", "git-lfs", "hg", "svn", "bzr", "gh", "glab", "hub", "tea", "jira", "slack", "op-cli",
		"pip-audit", "trivy", "grype", "syft", "cosign", "gitleaks", "semgrep", "snyk", "sonar-scanner", "codecov",
		"ollama", "huggingface-cli", "hf", "mc", "s3cmd", "rclone", "kaggle", "wandb", "mlflow", "dvc", "gsutil", "ngrok",
		"cloudflared", "tailscale", "wireguard", "wg", "sshuttle", "ansible-galaxy", "vagrant", "packer", "nix", "nix-shell", "nix-build", "guix",
	)
	register(handleGh, "gh", "glab")
	register(handleSSHAgentLike, "ssh-agent", "gpg-agent")
}

// handleFetch: curl/wget download; -o/-O write the destination; uploads of
// a local file (-T, -d @file, -F @file) are reads of that file, and a secret
// file is hard-denied — sending .env to a URL is exfiltration.
func handleFetch(a *analyzer, name string, args []word) result {
	r := network(name + " fetches a URL")
	if out, ok := flagValue(args, "-o", "--output", "-O", "--output-document"); ok {
		a.writeFiles(&r, []word{out}, true)
	}
	if hasFlag(args, "-O", "--remote-name") && name == "curl" {
		r.reason = joinReason(r.reason, "writes remote file name")
	}
	for i := range args {
		upload := uploadArg(args, i)
		if upload == "" || upload == "-" {
			continue
		}
		abs, _, err := a.ws.Resolve(upload)
		if err != nil {
			continue
		}
		r.reads = append(r.reads, abs)
		if a.ws.IsSecretFile(abs) || a.ws.IsProtected(abs) {
			return privilegeDenyNet(name + " uploads secret file " + a.display(abs))
		}
	}
	return r
}

// uploadArg returns the local file that the option at args[i] sends to the
// server: -T FILE, --post-file FILE, or -d/-F @FILE (and name=@FILE).
func uploadArg(args []word, i int) string {
	if i+1 >= len(args) {
		return ""
	}
	opt, val := args[i].text, args[i+1].text
	switch opt {
	case "-T", "--upload-file", "--post-file", "--body-file":
		return val
	case "-d", "--data", "--data-binary", "--data-raw", "-F", "--form":
		if _, after, ok := strings.Cut(val, "=@"); ok {
			return after
		}
		if strings.HasPrefix(val, "@") {
			return val[1:]
		}
	}
	return ""
}

// handleSSH: remote shells are network; the remote command is opaque.
func handleSSH(a *analyzer, name string, args []word) result {
	r := network(name + " connects to a remote host")
	if name == "scp" || name == "sftp" {
		for _, w := range nonFlags(args) {
			if strings.Contains(w.text, ":") {
				continue
			}
			if abs, _, err := a.ws.Resolve(w.text); err == nil && a.ws.IsSecretFile(abs) {
				return privilegeDenyNet(name + " copies secret file " + a.display(abs))
			}
		}
	}
	if i, ok := flagValue(args, "-i", "--identity"); ok {
		a.readFiles(&r, []word{i})
	}
	return r
}

// handleGh: gh/glab always reach the network; `auth token` prints a
// credential and is hard-denied, deletions are destructive.
func handleGh(a *analyzer, name string, args []word) result {
	nf := texts(nonFlags(args))
	joined := " " + strings.Join(nf, " ") + " "
	switch {
	case strings.Contains(joined, " auth token "), strings.Contains(joined, " auth login "), strings.Contains(joined, " auth refresh "), strings.Contains(joined, " auth setup-git "):
		return privilegeDenyNet(name + " auth touches stored credentials")
	case strings.Contains(joined, " delete "), strings.Contains(joined, " repo delete "):
		return destructive(name + " delete")
	case strings.Contains(joined, " secret set "), strings.Contains(joined, " secret list "):
		return privilegeDenyNet(name + " secret access")
	}
	return network(name + " " + first(args))
}

func handleSSHAgentLike(a *analyzer, name string, args []word) result {
	return privilegeDeny(name + " manages key agents")
}
