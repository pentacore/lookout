package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/pentacore/lookout/internal/buildinfo"
	"github.com/pentacore/lookout/internal/collect"
	"github.com/pentacore/lookout/internal/config"
	"github.com/pentacore/lookout/internal/platform"
	"github.com/pentacore/lookout/internal/state"
	"github.com/pentacore/lookout/internal/wire"
)

const enrollTimeout = time.Minute

// runEnroll exchanges a one-time enrollment token for agent credentials and
// saves them. Overwriting existing credentials or switching platform_url needs
// --force; re-enrolling resets the applied platform config to version 0.
func runEnroll(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("enroll", stderr)
	platformURL := flags.String("url", "", "platform URL, e.g. https://media.example.com (required)")
	token := flags.String("token", "", "enrollment token from the platform (required)")
	path := flags.String("config", defaultConfigPath(), "path to lookout.yaml")
	force := flags.Bool("force", false, "overwrite existing credentials or switch platform_url")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if *platformURL == "" || *token == "" {
		_, _ = fmt.Fprintln(stderr, "lookout enroll: --url and --token are required")
		flags.Usage()
		return 2
	}

	fail := func(format string, a ...any) int {
		msg := platform.Redact(fmt.Sprintf(format, a...), *token)
		_, _ = fmt.Fprintf(stderr, "lookout enroll: %s\n", msg)
		return 1
	}

	target := config.Config{PlatformURL: *platformURL}
	if err := target.Validate(); err != nil {
		return fail("%v", err)
	}

	cfg, err := config.Load(*path, config.DetectPlatform("/"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fail("%v", err)
	}
	setURL := cfg.PlatformURL == ""
	if !setURL && cfg.BaseURL() != target.BaseURL() {
		if !*force {
			return fail("platform_url in %s is %s; pass --force to switch to %s", *path, cfg.PlatformURL, target.BaseURL())
		}
		setURL = true
	}

	store := state.Store{Dir: cfg.StateDir}
	if !*force {
		creds, err := store.LoadCredentials()
		switch {
		case err == nil:
			return fail("already enrolled as %s; pass --force to re-enroll", creds.ServerUUID)
		case !errors.Is(err, fs.ErrNotExist):
			return fail("credentials in %s are unreadable (%v); pass --force to re-enroll", cfg.StateDir, err)
		}
	}

	if setURL {
		if err := config.SetPlatformURL(*path, target.BaseURL()); err != nil {
			return fail("%v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), enrollTimeout)
	defer cancel()
	host := collect.ReadHost("/")
	resp, err := platform.NewClient(target.BaseURL()).Enroll(ctx, wire.EnrollRequest{
		EnrollmentToken: *token,
		Hostname:        host.Hostname,
		OS:              host.OS,
		Kernel:          host.Kernel,
		Arch:            host.Arch,
		AgentVersion:    buildinfo.Version,
	})
	switch {
	case errors.Is(err, platform.ErrEnrollTokenGone):
		return fail("enrollment token expired or already used; create a new one in the platform")
	case errors.Is(err, platform.ErrEnrollTokenUnknown):
		return fail("enrollment token not recognised")
	case err != nil:
		return fail("enroll failed: %v", err)
	}
	if resp.ServerUUID == "" || !strings.HasPrefix(resp.AgentToken, resp.ServerUUID+".") {
		return fail("platform returned malformed credentials")
	}

	// The platform config goes first: a running agent that is polling for
	// credentials must not pick up the new identity next to the old one's
	// config version and checks.
	err = store.SavePlatformConfig(state.PlatformConfig{
		ConfigVersion:         0,
		ReportIntervalSeconds: int(platform.ClampInterval(resp.ReportIntervalSeconds, 0) / time.Second),
		Checks:                []wire.CheckConfig{},
	})
	if err != nil {
		return fail("enrolled as %s but cannot save the platform config: %v", resp.ServerUUID, err)
	}
	err = store.SaveCredentials(state.Credentials{ServerUUID: resp.ServerUUID, AgentToken: resp.AgentToken})
	if err != nil {
		return fail("enrolled as %s but cannot save the credentials: %v", resp.ServerUUID, err)
	}
	_, _ = fmt.Fprintf(stdout, "enrolled as %s\n", resp.ServerUUID)
	return 0
}
