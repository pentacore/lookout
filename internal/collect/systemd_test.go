package collect

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func systemdRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "run/systemd/system"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestFailedUnitsNoSystemd(t *testing.T) {
	called := false
	run := func(context.Context, string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	}

	units, err := FailedUnits(context.Background(), t.TempDir(), run)
	if units != nil || err != nil {
		t.Errorf("FailedUnits = %v, %v; want nil, nil", units, err)
	}
	if called {
		t.Error("runner was called without systemd")
	}
}

func TestFailedUnitsParses(t *testing.T) {
	var gotName string
	var gotArgs []string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotName, gotArgs = name, args
		return []byte("backup.service loaded failed failed Backup job\n\n● x.mount loaded failed failed X\n"), nil
	}

	units, err := FailedUnits(context.Background(), systemdRoot(t), run)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"backup.service", "x.mount"}; !slices.Equal(units, want) {
		t.Errorf("units = %v, want %v", units, want)
	}
	if gotName != "systemctl" {
		t.Errorf("command = %q, want systemctl", gotName)
	}
	if want := []string{"list-units", "--state=failed", "--plain", "--no-legend"}; !slices.Equal(gotArgs, want) {
		t.Errorf("args = %v, want %v", gotArgs, want)
	}
}

func TestFailedUnitsEmpty(t *testing.T) {
	run := func(context.Context, string, ...string) ([]byte, error) { return nil, nil }

	units, err := FailedUnits(context.Background(), systemdRoot(t), run)
	if err != nil {
		t.Fatal(err)
	}
	if units == nil || len(units) != 0 {
		t.Errorf("units = %#v, want empty non-nil slice", units)
	}
}

func TestFailedUnitsRunnerError(t *testing.T) {
	boom := errors.New("boom")
	run := func(context.Context, string, ...string) ([]byte, error) { return []byte("a.service x"), boom }

	units, err := FailedUnits(context.Background(), systemdRoot(t), run)
	if units != nil || !errors.Is(err, boom) {
		t.Errorf("FailedUnits = %v, %v; want nil, %v", units, err, boom)
	}
}

func TestFailedUnitsRunnerHasTimeout(t *testing.T) {
	var hasDeadline bool
	run := func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		_, hasDeadline = ctx.Deadline()
		return nil, nil
	}

	if _, err := FailedUnits(context.Background(), systemdRoot(t), run); err != nil {
		t.Fatal(err)
	}
	if !hasDeadline {
		t.Error("runner context has no deadline")
	}
}

func TestExecRunner(t *testing.T) {
	out, err := ExecRunner(context.Background(), "echo", "hello")
	if err != nil {
		t.Skipf("echo unavailable: %v", err)
	}
	if string(out) != "hello\n" {
		t.Errorf("out = %q, want %q", out, "hello\n")
	}
}
