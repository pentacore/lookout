// Command lookout is a host monitoring agent that reports to the platform.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/pentacore/lookout/internal/config"
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
	case "run":
		return runService(args[1:], stderr)
	case "enroll":
		return runEnroll(args[1:], stdout, stderr)
	case "check-config":
		return runCheckConfig(args[1:], stdout, stderr)
	case "version":
		return runVersion(args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "lookout: unknown command %q\n%s\n", args[0], usage)
		return 2
	}
}

// parseFlags parses args with fs and maps the outcome to an exit code. No
// subcommand takes positional arguments, so any are a usage error. ok is false
// when the caller should return code immediately.
func parseFlags(fs *flag.FlagSet, args []string) (code int, ok bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, false
		}
		return 2, false
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(fs.Output(), "lookout %s: unexpected argument %q\n", fs.Name(), fs.Arg(0))
		return 2, false
	}
	return 0, true
}

// defaultConfigPath is the config location for the platform this host runs.
func defaultConfigPath() string {
	return config.DefaultPath(config.DetectPlatform("/"))
}

// newFlagSet returns a flag set that reports errors on stderr instead of exiting.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}
