package collect

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pentacore/lookout/internal/wire"
)

// newDockerFake serves handler on a unix socket and returns the socket path.
// The path stays short because macOS limits socket paths to 104 bytes.
func newDockerFake(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "lk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "d.sock")

	server := httptest.NewUnstartedServer(handler)
	_ = server.Listener.Close()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return socket
}

const dockerListBody = `[{"Id":"a1","Names":["/postfix-mailcow"],"State":"running"},{"Id":"b2","Names":["/old"],"State":"exited"}]`

// dockerFakeHandler answers the list and inspect calls; inspectA lets a test
// change what the inspect of container a1 returns.
func dockerFakeHandler(t *testing.T, inspectA func(w http.ResponseWriter)) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("all") != "1" {
			t.Errorf("list query = %q, want all=1", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(dockerListBody))
	})
	mux.HandleFunc("/containers/a1/json", func(w http.ResponseWriter, _ *http.Request) {
		inspectA(w)
	})
	mux.HandleFunc("/containers/b2/json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"RestartCount":0,"State":{}}`))
	})
	return mux
}

func inspectHealthy(w http.ResponseWriter) {
	_, _ = w.Write([]byte(`{"RestartCount":4,"State":{"Health":{"Status":"healthy"}}}`))
}

func assertContainers(t *testing.T, got []wire.Container, wantRestarts int, wantHealth *string) {
	t.Helper()
	if len(got) != 2 {
		t.Fatalf("got %d containers, want 2: %+v", len(got), got)
	}
	first := got[0]
	if first.Name != "postfix-mailcow" || first.State != "running" || first.RestartCount != wantRestarts {
		t.Errorf("first = %+v", first)
	}
	switch {
	case wantHealth == nil && first.Health != nil:
		t.Errorf("first health = %q, want nil", *first.Health)
	case wantHealth != nil && (first.Health == nil || *first.Health != *wantHealth):
		t.Errorf("first health = %v, want %q", first.Health, *wantHealth)
	}
	second := got[1]
	if second.Name != "old" || second.State != "exited" || second.Health != nil || second.RestartCount != 0 {
		t.Errorf("second = %+v", second)
	}
}

func TestContainersNoSocket(t *testing.T) {
	client := &DockerClient{SocketPath: filepath.Join(t.TempDir(), "none.sock")}

	containers, err := client.Containers(context.Background())
	if containers != nil || err != nil {
		t.Errorf("Containers = %v, %v; want nil, nil", containers, err)
	}
}

func TestContainersListAndInspect(t *testing.T) {
	socket := newDockerFake(t, dockerFakeHandler(t, inspectHealthy))
	client := &DockerClient{SocketPath: socket}

	containers, err := client.Containers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	healthy := "healthy"
	assertContainers(t, containers, 4, &healthy)
}

func TestContainersEmptyListIsNonNil(t *testing.T) {
	socket := newDockerFake(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	client := &DockerClient{SocketPath: socket}

	containers, err := client.Containers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if containers == nil || len(containers) != 0 {
		t.Errorf("containers = %#v, want empty non-nil slice", containers)
	}
}

func TestContainersListErrorStatus(t *testing.T) {
	socket := newDockerFake(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	client := &DockerClient{SocketPath: socket}

	containers, err := client.Containers(context.Background())
	if containers != nil || err == nil {
		t.Errorf("Containers = %v, %v; want nil, error", containers, err)
	}
}

func TestContainersHungSocket(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	socket := newDockerFake(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	client := &DockerClient{SocketPath: socket, ListTimeout: 100 * time.Millisecond}

	start := time.Now()
	containers, err := client.Containers(context.Background())
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Containers took %v, want < 1s", elapsed)
	}
	if containers != nil || err == nil {
		t.Errorf("Containers = %v, %v; want nil, error", containers, err)
	}
}

func TestContainersInspectFailureUsesCache(t *testing.T) {
	var mu sync.Mutex
	inspectOK := true
	socket := newDockerFake(t, dockerFakeHandler(t, func(w http.ResponseWriter) {
		mu.Lock()
		ok := inspectOK
		mu.Unlock()
		if !ok {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		inspectHealthy(w)
	}))
	client := &DockerClient{SocketPath: socket}
	healthy := "healthy"

	first, err := client.Containers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertContainers(t, first, 4, &healthy)

	mu.Lock()
	inspectOK = false
	mu.Unlock()
	second, err := client.Containers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertContainers(t, second, 4, &healthy)
}

func TestContainersInspectFailureWithoutCache(t *testing.T) {
	socket := newDockerFake(t, dockerFakeHandler(t, func(w http.ResponseWriter) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	client := &DockerClient{SocketPath: socket}

	containers, err := client.Containers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertContainers(t, containers, 0, nil)
}

func TestContainersNameFallsBackToShortID(t *testing.T) {
	socket := newDockerFake(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/containers/json" {
			_, _ = w.Write([]byte(`[{"Id":"0123456789abcdef","Names":[],"State":"created"}]`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	client := &DockerClient{SocketPath: socket}

	containers, err := client.Containers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 || containers[0].Name != "0123456789ab" {
		t.Errorf("containers = %+v, want name 0123456789ab", containers)
	}
}

// dockerHungInspectHandler answers the list call but never answers an inspect
// until release is closed.
func dockerHungInspectHandler(release <-chan struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/containers/json" {
			_, _ = w.Write([]byte(dockerListBody))
			return
		}
		<-release
	})
}

func TestContainersHungInspectWithoutCache(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	socket := newDockerFake(t, dockerHungInspectHandler(release))
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	client := &DockerClient{SocketPath: socket, ListTimeout: time.Second, InspectTimeout: 100 * time.Millisecond}

	start := time.Now()
	containers, err := client.Containers(context.Background())
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("Containers took %v, want well within ListTimeout+InspectTimeout", elapsed)
	}
	if err != nil {
		t.Fatal(err)
	}
	assertContainers(t, containers, 0, nil)
}

func TestContainersHungInspectUsesCache(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	hung := false
	healthy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		isHung := hung
		mu.Unlock()
		if isHung {
			dockerHungInspectHandler(release).ServeHTTP(w, r)
			return
		}
		dockerFakeHandler(t, inspectHealthy).ServeHTTP(w, r)
	})
	socket := newDockerFake(t, healthy)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	client := &DockerClient{SocketPath: socket, ListTimeout: time.Second, InspectTimeout: 100 * time.Millisecond}
	healthyStatus := "healthy"

	first, err := client.Containers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertContainers(t, first, 4, &healthyStatus)

	mu.Lock()
	hung = true
	mu.Unlock()
	start := time.Now()
	second, err := client.Containers(context.Background())
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("Containers took %v, want well within ListTimeout+InspectTimeout", elapsed)
	}
	if err != nil {
		t.Fatal(err)
	}
	assertContainers(t, second, 4, &healthyStatus)
}

func TestContainersOversizedResponseIsError(t *testing.T) {
	socket := newDockerFake(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("["))
		chunk := strings.Repeat(" ", 1<<20)
		for range 17 {
			_, _ = w.Write([]byte(chunk))
		}
		_, _ = w.Write([]byte("]"))
	}))
	client := &DockerClient{SocketPath: socket}

	containers, err := client.Containers(context.Background())
	if containers != nil || err == nil {
		t.Errorf("Containers = %v, %v; want nil, error", containers, err)
	}
}
