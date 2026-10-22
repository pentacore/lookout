package checks

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pentacore/lookout/internal/wire"
)

const (
	defaultBudget      = 10 * time.Second
	defaultConcurrency = 8
)

// Runner executes check configs concurrently under a per-check time budget.
// A Runner holds in-flight state and must not be copied after first use.
type Runner struct {
	Registry Registry
	Env      Env
	// Budget bounds each check; zero means 10s.
	Budget time.Duration
	// Concurrency bounds simultaneously running checks; zero means 8.
	Concurrency int
	// Version is the agent version quoted in unsupported-type messages.
	Version string

	mu       sync.Mutex
	inflight map[string]bool
}

// Run executes cfgs and returns one result per config, in input order. The
// returned slice is never nil and no result has a nil Observed map.
func (r *Runner) Run(ctx context.Context, cfgs []wire.CheckConfig) []wire.CheckResult {
	results := make([]wire.CheckResult, len(cfgs))

	concurrency := r.Concurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}

	slots := make(chan struct{}, concurrency)

	var wg sync.WaitGroup

	for i, cfg := range cfgs {
		wg.Add(1)

		go func() {
			defer wg.Done()

			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				results[i] = result(cfg, Outcome{Status: wire.StatusError, Message: "cancelled before start: " + ctx.Err().Error()})

				return
			}

			defer func() { <-slots }()

			results[i] = result(cfg, r.runOne(ctx, cfg))
		}()
	}

	wg.Wait()

	return results
}

func result(cfg wire.CheckConfig, out Outcome) wire.CheckResult {
	if out.Observed == nil {
		out.Observed = map[string]any{}
	}

	return wire.CheckResult{
		ID:       cfg.ID,
		Type:     cfg.Type,
		Status:   out.Status,
		Message:  out.Message,
		Observed: out.Observed,
	}
}

func (r *Runner) runOne(ctx context.Context, cfg wire.CheckConfig) Outcome {
	fn, ok := r.Registry[cfg.Type]
	if !ok {
		return Outcome{
			Status:  wire.StatusUnsupported,
			Message: fmt.Sprintf("check type %q is not supported by lookout %s", cfg.Type, r.Version),
		}
	}

	if !r.acquire(cfg.ID) {
		return Outcome{
			Status:  wire.StatusError,
			Message: "previous run still in progress (hung filesystem?)",
		}
	}

	budget := r.Budget
	if budget <= 0 {
		budget = defaultBudget
	}

	checkCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	done := make(chan Outcome, 1)

	go func() {
		defer r.release(cfg.ID)
		defer func() {
			if v := recover(); v != nil {
				done <- Outcome{Status: wire.StatusError, Message: fmt.Sprintf("check panicked: %v", v)}
			}
		}()

		done <- fn(checkCtx, r.Env, cfg.Params)
	}()

	select {
	case out := <-done:
		return out
	case <-checkCtx.Done():
		if ctx.Err() != nil {
			return Outcome{Status: wire.StatusError, Message: "cancelled: " + ctx.Err().Error()}
		}

		return Outcome{Status: wire.StatusError, Message: fmt.Sprintf("timed out after %s", budget)}
	}
}

// acquire marks id as running and reports false if it already is.
func (r *Runner) acquire(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.inflight == nil {
		r.inflight = map[string]bool{}
	}

	if r.inflight[id] {
		return false
	}

	r.inflight[id] = true

	return true
}

func (r *Runner) release(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.inflight, id)
}
