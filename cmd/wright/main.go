// Command wright is a coding-agent terminal harness: approval-first
// permissions, an OS sandbox around every shell command, secrets that never
// leave the machine, and a tamper-evident audit trail.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/richardwooding/wright/internal/cli"
	"github.com/richardwooding/wright/internal/tuiwire"
)

// Set by GoReleaser via -ldflags "-X main.version=… -X main.commit=… -X main.date=…".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code, err := cli.Main(ctx, os.Args[1:], cli.BuildInfo{Version: version, Commit: commit, Date: date}, tuiwire.Interactive)
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "wright: %v\n", err)
	}
	// stop() before exit so the signal handler is restored even on the error path.
	stop()
	os.Exit(code)
}
