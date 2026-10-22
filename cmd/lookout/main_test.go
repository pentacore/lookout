package main

import (
	"bytes"
	"strings"
	"testing"
)

const usageLine = "usage: lookout <run|enroll|version|check-config>"

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	if got := stdout.String(); got != "lookout dev\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestRunUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"unknown command", []string{"nope"}},
		{"no args", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, &stdout, &stderr); code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), usageLine) {
				t.Fatalf("stderr = %q, want usage line", stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
		})
	}
}

func TestRunVersionRejectsExtraArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version", "--bogus"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRunRejectsPositionalArgs(t *testing.T) {
	env := newTestEnv(t, "https://media.example.com")
	tests := []struct {
		name string
		args []string
	}{
		{"version", []string{"version", "foo"}},
		{"run", []string{"run", "--config", env.config, "foo"}},
		{"check-config", []string{"check-config", "--config", env.config, "foo"}},
		{"enroll", []string{"enroll", "--config", env.config, "--url", "https://x.example", "--token", enrollToken, "foo"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(tt.args...)
			if code != 2 || stdout != "" || !strings.Contains(stderr, `unexpected argument "foo"`) {
				t.Fatalf("exit code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
		})
	}
}
