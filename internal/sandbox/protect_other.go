//go:build !linux

package sandbox

// protect is a no-op off Linux: the landlock helper, and with it the mount
// namespace that carries the read-only bind mounts, exists only there.
// seatbelt expresses the same protection in its profile instead.
func protect([]string) error { return nil }

// probeProtection reports that there is nothing to probe off Linux.
func probeProtection(string) error { return ErrUnsupported }

// CurrentNS returns the empty string off Linux, where there are no
// namespaces to compare.
func CurrentNS() string { return "" }
