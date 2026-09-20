//go:build unix

package diag

import (
	"os"
	"os/signal"
	"syscall"
)

// DumpSignal is the signal that writes a dump. SIGUSR1 is the conventional
// "tell me what you are doing" signal and, unlike SIGQUIT, it neither kills
// the process nor prints to a terminal a full-screen UI is using.
const DumpSignal = "SIGUSR1"

// notifyDump calls dump on every SIGUSR1 until the returned function is
// called. The handler runs on its own goroutine, so a dump that takes a
// moment does not delay the next signal being delivered.
func notifyDump(dump func()) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
				dump()
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}
