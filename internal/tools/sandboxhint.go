package tools

import (
	"fmt"
	"strings"

	"github.com/richardwooding/wright/internal/sandbox"
)

// sandboxSignatures are the messages a command emits when it ran into the
// sandbox rather than a real problem with the machine.
var (
	noNetworkSignatures = []string{
		"network is unreachable",
		"could not connect to server",
		"temporary failure in name resolution",
		"could not resolve host",
		"name or service not known",
		"connection refused",
		"curl: (6)",
		"curl: (7)",
	}
	readOnlySignatures = []string{
		"read-only file system",
		"erofs",
	}
)

// sandboxHints explains a failure the sandbox caused, so the model is told
// what happened instead of spending a dozen tool calls discovering it. A
// command that fails because wright withheld something should say which
// thing, and what would grant it.
func sandboxHints(out string, spec sandbox.Spec, backend string) []string {
	if backend == "" || backend == "none" {
		return nil
	}
	low := strings.ToLower(out)
	var notes []string
	if !spec.Network && containsAny(low, noNetworkSignatures) {
		notes = append(notes, "this call ran without network access, which is why the connection failed. "+
			"Ask for it: the approval prompt grants the network when you say the command needs it, "+
			"and a saved rule needs the +net suffix. Do not try to route around it.")
	}
	if containsAny(low, readOnlySignatures) {
		notes = append(notes, fmt.Sprintf("a write hit a read-only mount: inside the sandbox only %s %s writable. "+
			"Approving an install makes that tool's prefix writable for the call; nothing else here is.",
			writableList(spec), isAre(len(spec.ReadWrite))))
	}
	return notes
}

func containsAny(low string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(low, n) {
			return true
		}
	}
	return false
}

// writableList names what the call could write, shortened so the note stays
// one line on a normal terminal.
func writableList(spec sandbox.Spec) string {
	const max = 4
	rw := spec.ReadWrite
	if len(rw) == 0 {
		return "the workspace"
	}
	if len(rw) > max {
		return strings.Join(rw[:max], ", ") + fmt.Sprintf(" and %d more", len(rw)-max)
	}
	return strings.Join(rw, ", ")
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}
