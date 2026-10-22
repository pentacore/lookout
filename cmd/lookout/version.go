package main

import (
	"fmt"
	"io"

	"github.com/pentacore/lookout/internal/buildinfo"
)

// runVersion prints "lookout {version}".
func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("version", stderr)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	_, _ = fmt.Fprintf(stdout, "lookout %s\n", buildinfo.Version)
	return 0
}
