package main

import (
	"strings"
	"testing"
)

func TestCheckConfigOK(t *testing.T) {
	env := newTestEnv(t, "https://media.example.com")

	code, stdout, stderr := runCLI("check-config", "--config", env.config)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"config:       " + env.config + "\n",
		"platform:     systemd\n",
		"platform_url: https://media.example.com\n",
		"state_dir:    " + env.store.Dir + "\n",
		"runtime_dir:  " + env.dir + "/run\n",
		"auto_update:  true\n",
		"enrolled:     no\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if stderr != "" {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestCheckConfigEnrolled(t *testing.T) {
	env := newTestEnv(t, "https://media.example.com", "auto_update: false")
	env.saveCredentials("9f0c1e")

	code, stdout, stderr := runCLI("check-config", "--config", env.config)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "enrolled:     yes (server 9f0c1e)\n") || !strings.Contains(stdout, "auto_update:  false\n") {
		t.Errorf("stdout:\n%s", stdout)
	}
	if strings.Contains(stdout, "secret") {
		t.Error("stdout contains the agent token")
	}
}

func TestCheckConfigUnreadableCredentials(t *testing.T) {
	env := newTestEnv(t, "https://media.example.com")
	env.saveCredentials("9f0c1e")
	env.corruptCredentials()

	code, stdout, _ := runCLI("check-config", "--config", env.config)
	if code != 0 || !strings.Contains(stdout, "enrolled:     no (credentials unreadable") {
		t.Fatalf("exit code = %d, stdout:\n%s", code, stdout)
	}
}

func TestCheckConfigWarnsAboutUnknownKeys(t *testing.T) {
	env := newTestEnv(t, "https://media.example.com", "plattform_url: oops")

	code, _, stderr := runCLI("check-config", "--config", env.config)
	if code != 0 || !strings.Contains(stderr, `warning: unknown key "plattform_url"`) {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
}

func TestCheckConfigInvalid(t *testing.T) {
	tests := []struct {
		name, url, want string
	}{
		{"plain http", "http://x.example", "https"},
		{"empty", "", "platform_url is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t, tt.url)

			code, stdout, stderr := runCLI("check-config", "--config", env.config)
			if code != 1 || stdout != "" || !strings.Contains(stderr, tt.want) {
				t.Fatalf("exit code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
		})
	}
}

func TestCheckConfigMissingFile(t *testing.T) {
	env := newTestEnv(t, "https://media.example.com")

	code, stdout, stderr := runCLI("check-config", "--config", env.dir+"/nope.yaml")
	if code != 1 || stdout != "" || stderr == "" {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}
