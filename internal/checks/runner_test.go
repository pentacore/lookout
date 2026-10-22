package checks

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pentacore/lookout/internal/wire"
)

func okFunc(context.Context, Env, json.RawMessage) Outcome {
	return Outcome{Status: wire.StatusOK, Message: "fine"}
}

func TestRunnerUnknownType(t *testing.T) {
	r := &Runner{Registry: Registry{}, Version: "1.2.3"}

	got := r.Run(context.Background(), []wire.CheckConfig{{ID: "a", Type: "cpu_temp"}})

	if got[0].Status != wire.StatusUnsupported {
		t.Fatalf("status = %q, want unsupported", got[0].Status)
	}

	want := `check type "cpu_temp" is not supported by lookout 1.2.3`
	if got[0].Message != want {
		t.Fatalf("message = %q, want %q", got[0].Message, want)
	}
}

func TestRunnerPreservesOrder(t *testing.T) {
	r := &Runner{Registry: Registry{
		"slow": func(ctx context.Context, _ Env, _ json.RawMessage) Outcome {
			time.Sleep(30 * time.Millisecond)

			return Outcome{Status: wire.StatusOK}
		},
		"fast": okFunc,
	}}

	cfgs := []wire.CheckConfig{
		{ID: "1", Type: "slow"},
		{ID: "2", Type: "fast"},
		{ID: "3", Type: "missing"},
	}

	got := r.Run(context.Background(), cfgs)

	if len(got) != len(cfgs) {
		t.Fatalf("len = %d, want %d", len(got), len(cfgs))
	}

	for i, cfg := range cfgs {
		if got[i].ID != cfg.ID || got[i].Type != cfg.Type {
			t.Errorf("result %d = %s/%s, want %s/%s", i, got[i].ID, got[i].Type, cfg.ID, cfg.Type)
		}
	}
}

func TestRunnerPanic(t *testing.T) {
	r := &Runner{Registry: Registry{
		"boom": func(context.Context, Env, json.RawMessage) Outcome { panic("kaput") },
	}}

	got := r.Run(context.Background(), []wire.CheckConfig{{ID: "a", Type: "boom"}})

	if got[0].Status != wire.StatusError || !strings.Contains(got[0].Message, "panicked: kaput") {
		t.Fatalf("got %s %q", got[0].Status, got[0].Message)
	}
}

func TestRunnerTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	r := &Runner{
		Budget: 50 * time.Millisecond,
		Registry: Registry{
			"hang": func(context.Context, Env, json.RawMessage) Outcome {
				<-release

				return Outcome{Status: wire.StatusOK}
			},
		},
	}

	start := time.Now()
	got := r.Run(context.Background(), []wire.CheckConfig{{ID: "a", Type: "hang"}})

	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("took %s, want < 200ms", elapsed)
	}

	if got[0].Status != wire.StatusError || got[0].Message != "timed out after 50ms" {
		t.Fatalf("got %s %q", got[0].Status, got[0].Message)
	}
}

func TestRunnerStillRunning(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 4)

	var calls int

	r := &Runner{
		Budget: 30 * time.Millisecond,
		Registry: Registry{
			"hang": func(context.Context, Env, json.RawMessage) Outcome {
				calls++

				started <- struct{}{}
				<-release

				return Outcome{Status: wire.StatusOK}
			},
		},
	}

	cfgs := []wire.CheckConfig{{ID: "same", Type: "hang"}}

	first := r.Run(context.Background(), cfgs)
	if !strings.Contains(first[0].Message, "timed out") {
		t.Fatalf("first message = %q", first[0].Message)
	}

	second := r.Run(context.Background(), cfgs)
	if second[0].Status != wire.StatusError || !strings.Contains(second[0].Message, "previous run still in progress") {
		t.Fatalf("second = %s %q", second[0].Status, second[0].Message)
	}

	close(release)

	// Once the hung call returns, the ID is free again.
	deadline := time.After(2 * time.Second)

	for {
		r.mu.Lock()
		busy := r.inflight["same"]
		r.mu.Unlock()

		if !busy {
			break
		}

		select {
		case <-deadline:
			t.Fatal("in-flight flag never cleared")
		case <-time.After(5 * time.Millisecond):
		}
	}

	if calls != 1 {
		t.Fatalf("check invoked %d times, want 1", calls)
	}
}

func TestRunnerBadParams(t *testing.T) {
	type params struct {
		Path string `json:"path"`
	}

	r := &Runner{Registry: Registry{
		"needs_path": func(_ context.Context, _ Env, raw json.RawMessage) Outcome {
			if _, err := decodeParams[params](raw); err != nil {
				return invalidParams(err)
			}

			return Outcome{Status: wire.StatusOK}
		},
	}}

	got := r.Run(context.Background(), []wire.CheckConfig{
		{ID: "a", Type: "needs_path", Params: json.RawMessage(`{"path":5}`)},
	})

	if got[0].Status != wire.StatusError || !strings.HasPrefix(got[0].Message, "invalid params") {
		t.Fatalf("got %s %q", got[0].Status, got[0].Message)
	}
}

func TestRunnerEmpty(t *testing.T) {
	r := &Runner{}

	got := r.Run(context.Background(), nil)
	if got == nil || len(got) != 0 {
		t.Fatalf("got %#v, want non-nil empty slice", got)
	}

	raw, err := json.Marshal(got)
	if err != nil || string(raw) != "[]" {
		t.Fatalf("json = %s, err = %v", raw, err)
	}
}

func TestRunnerObservedNeverNil(t *testing.T) {
	r := &Runner{Registry: Registry{"ok": okFunc}}

	got := r.Run(context.Background(), []wire.CheckConfig{
		{ID: "a", Type: "ok"},
		{ID: "b", Type: "missing"},
	})

	for _, res := range got {
		raw, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}

		if !strings.Contains(string(raw), `"observed":{}`) {
			t.Errorf("%s: json = %s, want observed {}", res.ID, raw)
		}
	}
}

func TestDecodeParams(t *testing.T) {
	type params struct {
		Path string `json:"path"`
	}

	tests := []struct {
		name    string
		raw     json.RawMessage
		want    string
		wantErr bool
	}{
		{"empty", nil, "", false},
		{"null", json.RawMessage(`null`), "", false},
		{"value", json.RawMessage(`{"path":"/x"}`), "/x", false},
		{"unknown field ignored", json.RawMessage(`{"path":"/x","extra":1}`), "/x", false},
		{"wrong type", json.RawMessage(`{"path":5}`), "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeParams[params](tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}

			if got.Path != tt.want {
				t.Fatalf("path = %q, want %q", got.Path, tt.want)
			}
		})
	}
}

func TestMissingParam(t *testing.T) {
	out := missingParam("path")
	if out.Status != wire.StatusError || out.Message != "missing required param: path" {
		t.Fatalf("got %s %q", out.Status, out.Message)
	}
}

func TestEnvHostPath(t *testing.T) {
	env := DefaultEnv("/host", "/run/lookout")
	if got := env.HostPath("/etc/os-release"); got != "/host/etc/os-release" {
		t.Fatalf("got %q", got)
	}
}
