// Command lookout is a host monitoring agent that reports to the platform.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

const usage = "usage: lookout <run|enroll|version|check-config>"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches a subcommand and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		return runVersion(args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "lookout: unknown command %q\n%s\n", args[0], usage)
		return 2
	}
}

// parseFlags parses args with fs and maps the outcome to an exit code. ok is
// false when the caller should return code immediately.
func parseFlags(fs *flag.FlagSet, args []string) (code int, ok bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, false
		}
		return 2, false
	}
	return 0, true
}

// newFlagSet returns a flag set that reports errors on stderr instead of exiting.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}
