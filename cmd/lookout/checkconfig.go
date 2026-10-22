package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/pentacore/lookout/internal/config"
	"github.com/pentacore/lookout/internal/state"
)

// runCheckConfig validates the config and prints a summary. install.sh greps
// the output for `^enrolled: *yes`, so the column layout is part of the
// interface.
func runCheckConfig(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("check-config", stderr)
	path := flags.String("config", defaultConfigPath(), "path to lookout.yaml")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}

	cfg, err := config.Load(*path, config.DetectPlatform("/"))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "lookout check-config: %v\n", err)
		return 1
	}
	for _, key := range cfg.UnknownKeys {
		_, _ = fmt.Fprintf(stderr, "warning: unknown key %q\n", key)
	}
	if err := cfg.Validate(); err != nil {
		_, _ = fmt.Fprintf(stderr, "lookout check-config: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "config:       %s\n", cfg.Path)
	_, _ = fmt.Fprintf(stdout, "platform:     %s\n", cfg.Platform)
	_, _ = fmt.Fprintf(stdout, "platform_url: %s\n", cfg.PlatformURL)
	_, _ = fmt.Fprintf(stdout, "state_dir:    %s\n", cfg.StateDir)
	_, _ = fmt.Fprintf(stdout, "runtime_dir:  %s\n", cfg.RuntimeDir)
	_, _ = fmt.Fprintf(stdout, "auto_update:  %t\n", cfg.AutoUpdate)
	_, _ = fmt.Fprintf(stdout, "enrolled:     %s\n", enrollmentSummary(state.Store{Dir: cfg.StateDir}))
	return 0
}

// enrollmentSummary describes the saved credentials without revealing the
// agent token.
func enrollmentSummary(store state.Store) string {
	creds, err := store.LoadCredentials()
	switch {
	case err == nil:
		return "yes (server " + creds.ServerUUID + ")"
	case errors.Is(err, fs.ErrNotExist):
		return "no"
	default:
		return "no (credentials unreadable: " + err.Error() + ")"
	}
}
