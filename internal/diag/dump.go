package diag

import (
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
	"time"
)

// render lays the report out as plain text: a header, the caller's sections,
// then every goroutine's stack. Plain text because this is read in a
// terminal, pasted into an issue and grepped — and because a dump written
// while the process is misbehaving should need nothing to open it.
func render(now time.Time, sections []Section, stacks bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "wright diagnostics — %s\n", now.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "pid %d · go %s · %s/%s · %d goroutines · GOMAXPROCS %d\n",
		os.Getpid(), runtime.Version(), runtime.GOOS, runtime.GOARCH,
		runtime.NumGoroutine(), runtime.GOMAXPROCS(0))
	for _, s := range sections {
		fmt.Fprintf(&b, "\n== %s ==\n", s.Title)
		if len(s.Lines) == 0 {
			b.WriteString("  (none)\n")
			continue
		}
		for _, l := range s.Lines {
			b.WriteString("  " + l + "\n")
		}
	}
	if stacks {
		b.WriteString("\n== goroutines ==\n")
		writeStacks(&b)
	}
	return b.String()
}

// writeStacks appends every goroutine's stack in the readable form (debug=2,
// the shape a panic prints), which is what makes "blocked on a channel
// nobody sends to" visible at a glance.
func writeStacks(b *strings.Builder) {
	p := pprof.Lookup("goroutine")
	if p == nil {
		b.WriteString("  (goroutine profile unavailable)\n")
		return
	}
	if err := p.WriteTo(b, 2); err != nil {
		fmt.Fprintf(b, "  (goroutine profile failed: %v)\n", err)
	}
}
