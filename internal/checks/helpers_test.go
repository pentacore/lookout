package checks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pentacore/lookout/internal/collect"
	"github.com/pentacore/lookout/internal/wire"
)

// testNow is the fixed clock every test environment reports.
var testNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

const testMountInfo = `1 0 8:1 / / rw - ext4 /dev/sda1 rw
2 1 0:40 / /mnt/backups rw - nfs4 nas:/backups rw
3 1 8:17 / /mnt/appdata\040cache rw - ext4 /dev/sdb1 rw
`

// newTestEnv returns an Env rooted at a fresh temp dir whose
// proc/self/mountinfo holds mountinfo. Tests may replace Now.
func newTestEnv(t *testing.T, mountinfo string) Env {
	t.Helper()

	root := t.TempDir()
	mountinfoPath := filepath.Join(root, "proc/self/mountinfo")

	if err := os.MkdirAll(filepath.Dir(mountinfoPath), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(mountinfoPath, []byte(mountinfo), 0o644); err != nil {
		t.Fatal(err)
	}

	return Env{
		Root: root,
		Mounts: func() (collect.MountTable, error) {
			return collect.ReadMountTable(root)
		},
		Now: func() time.Time { return testNow },
	}
}

// checkCase is one row of a check's table test.
type checkCase struct {
	name         string
	params       string
	wantStatus   wire.CheckStatus
	wantMessage  string
	wantContains string
	wantObserved map[string]any
}

func (c checkCase) assert(t *testing.T, got Outcome) {
	t.Helper()

	if got.Status != c.wantStatus {
		t.Errorf("status = %q (%s), want %q", got.Status, got.Message, c.wantStatus)
	}

	if c.wantMessage != "" && got.Message != c.wantMessage {
		t.Errorf("message = %q, want %q", got.Message, c.wantMessage)
	}

	if c.wantContains != "" && !strings.Contains(got.Message, c.wantContains) {
		t.Errorf("message = %q, want it to contain %q", got.Message, c.wantContains)
	}

	if c.wantObserved != nil && !reflect.DeepEqual(got.Observed, c.wantObserved) {
		t.Errorf("observed = %#v, want %#v", got.Observed, c.wantObserved)
	}
}

func runCheck(fn Func, env Env, params string) Outcome {
	return fn(context.Background(), env, json.RawMessage(params))
}

func TestDefaultRegistryHasFilesystemChecks(t *testing.T) {
	registry := DefaultRegistry()

	for _, name := range []string{"mount_present", "path_on_mount", "file_freshness"} {
		if registry[name] == nil {
			t.Errorf("DefaultRegistry has no %q", name)
		}
	}
}
