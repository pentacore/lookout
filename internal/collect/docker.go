package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pentacore/lookout/internal/wire"
)

const (
	defaultDockerListTimeout    = 3 * time.Second
	defaultDockerInspectTimeout = 5 * time.Second
	dockerInspectConcurrency    = 4
	dockerShortIDLength         = 12
	dockerMaxResponseBytes      = 16 << 20
)

// DockerClient lists containers over the Docker unix socket using the
// unversioned API. Zero timeouts default to 3 s for the list call and 5 s in
// total for all inspect calls, so one call takes at most ListTimeout +
// InspectTimeout (8 s by default). It is safe for concurrent use.
type DockerClient struct {
	SocketPath     string
	ListTimeout    time.Duration
	InspectTimeout time.Duration

	once      sync.Once
	transport *http.Transport

	mu    sync.Mutex
	cache map[string]inspectInfo // keyed by container ID
}

type inspectInfo struct {
	restartCount int
	health       *string
}

type dockerListEntry struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
	State string   `json:"State"`
}

type dockerInspect struct {
	RestartCount int `json:"RestartCount"`
	State        struct {
		Health *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
}

// Containers returns every container, running or not. It returns nil, nil
// when the socket does not exist, and nil with an error when the list call
// fails or times out. RestartCount and health come from a per-container
// inspect; a failed inspect reuses the last known values for that container,
// or 0 and no health if there are none. A hung inspect therefore never
// turns the result into an error; only the list call decides that.
func (d *DockerClient) Containers(ctx context.Context) ([]wire.Container, error) {
	if _, err := os.Stat(d.SocketPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var entries []dockerListEntry
	listCtx, cancelList := context.WithTimeout(ctx, orDefault(d.ListTimeout, defaultDockerListTimeout))
	defer cancelList()
	if err := d.getJSON(listCtx, "/containers/json?all=1", &entries); err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	inspectCtx, cancelInspect := context.WithTimeout(ctx, orDefault(d.InspectTimeout, defaultDockerInspectTimeout))
	defer cancelInspect()
	infos := make([]inspectInfo, len(entries))
	sem := make(chan struct{}, dockerInspectConcurrency)
	var wg sync.WaitGroup
	for i, entry := range entries {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			infos[i] = d.inspect(inspectCtx, entry.ID)
		}()
	}
	wg.Wait()

	containers := make([]wire.Container, len(entries))
	live := make(map[string]struct{}, len(entries))
	for i, entry := range entries {
		live[entry.ID] = struct{}{}
		containers[i] = wire.Container{
			Name:         containerName(entry),
			State:        entry.State,
			Health:       infos[i].health,
			RestartCount: infos[i].restartCount,
		}
	}
	d.forgetExcept(live)
	return containers, nil
}

// inspect returns the restart count and health of one container, falling back
// to the cached values when the request fails.
func (d *DockerClient) inspect(ctx context.Context, id string) inspectInfo {
	var body dockerInspect
	if err := d.getJSON(ctx, "/containers/"+id+"/json", &body); err != nil {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.cache[id]
	}

	info := inspectInfo{restartCount: body.RestartCount}
	if body.State.Health != nil && body.State.Health.Status != "" {
		status := body.State.Health.Status
		info.health = &status
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cache == nil {
		d.cache = map[string]inspectInfo{}
	}
	d.cache[id] = info
	return info
}

// forgetExcept drops cached inspect results for containers that are gone.
func (d *DockerClient) forgetExcept(live map[string]struct{}) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id := range d.cache {
		if _, ok := live[id]; !ok {
			delete(d.cache, id)
		}
	}
}

func (d *DockerClient) getJSON(ctx context.Context, path string, into any) error {
	d.once.Do(func() {
		d.transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", d.SocketPath)
			},
			DisableKeepAlives: true,
		}
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Transport: d.transport}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("docker API %s: %s", path, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, dockerMaxResponseBytes)).Decode(into)
}

// containerName is the first name without its leading slash, or the short ID
// for a container that has none.
func containerName(e dockerListEntry) string {
	if len(e.Names) > 0 {
		return strings.TrimPrefix(e.Names[0], "/")
	}
	return e.ID[:min(len(e.ID), dockerShortIDLength)]
}

func orDefault(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}
