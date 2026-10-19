//go:build linux

package pdfextract

import "golang.org/x/sys/unix"

func applyLimits() error {
	for _, limit := range []struct {
		resource int
		value    uint64
	}{
		// Go reserves considerable virtual address space at startup. The data
		// limit separately caps writable anonymous memory (including Go heap)
		// on Linux, while AS also bounds any further address-space mappings.
		{unix.RLIMIT_AS, 2 << 30},
		{unix.RLIMIT_DATA, 256 << 20},
		{unix.RLIMIT_CPU, 2},
		{unix.RLIMIT_CORE, 0},
		{unix.RLIMIT_NOFILE, 32},
	} {
		if err := unix.Setrlimit(limit.resource, &unix.Rlimit{Cur: limit.value, Max: limit.value}); err != nil {
			return err
		}
	}
	return nil
}
