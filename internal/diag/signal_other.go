//go:build !unix

package diag

// DumpSignal names the dump signal; there is none on this platform.
const DumpSignal = ""

// notifyDump does nothing where there is no SIGUSR1. The HTTP endpoint and
// an explicit Dump still work.
func notifyDump(func()) func() { return func() {} }
