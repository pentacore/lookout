package collect

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/pentacore/lookout/internal/wire"
)

// Options configure a Collector. Zero values become defaults: Root "/",
// RealStatfs, ExecRunner, {Root}/var/run/docker.sock and slog.Default().
type Options struct {
	Root         string
	Statfs       StatfsFunc
	RunCommand   CommandRunner
	DockerSocket string
	Logger       *slog.Logger
}

// Collector gathers the host identity and metrics for one report.
type Collector struct {
	root       string
	runCommand CommandRunner
	logger     *slog.Logger
	cpu        *CPUSampler
	filesystem *FilesystemCollector
	docker     *DockerClient
}

// New returns a Collector with the defaults from Options filled in.
func New(o Options) *Collector {
	if o.Root == "" {
		o.Root = "/"
	}
	if o.Statfs == nil {
		o.Statfs = RealStatfs
	}
	if o.RunCommand == nil {
		o.RunCommand = ExecRunner
	}
	if o.DockerSocket == "" {
		o.DockerSocket = filepath.Join(o.Root, "var/run/docker.sock")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Collector{
		root:       o.Root,
		runCommand: o.RunCommand,
		logger:     o.Logger,
		cpu:        NewCPUSampler(o.Root),
		filesystem: &FilesystemCollector{Root: o.Root, Statfs: o.Statfs, Logger: o.Logger},
		docker:     &DockerClient{SocketPath: o.DockerSocket},
	}
}

// Collect gathers the host and its metrics. Each source has its own timeout,
// and one that fails leaves its field at the zero value (or null for the
// optional ones) and logs one warning; it never aborts the report. Collect
// must not be called concurrently.
func (c *Collector) Collect(ctx context.Context) (wire.Host, wire.Metrics) {
	var metrics wire.Metrics

	var wg sync.WaitGroup
	wg.Go(func() {
		metrics.Filesystems = c.filesystems()
	})
	wg.Go(func() {
		metrics.FailedUnits = c.failedUnits(ctx)
	})
	wg.Go(func() {
		metrics.Containers = c.containers(ctx)
	})

	host := ReadHost(c.root)
	metrics.CPUPercent = warnOnError(c, "cpu", c.cpu.Percent)
	metrics.Load = warnOnError(c, "load", func() ([3]float64, error) { return ReadLoad(c.root) })
	metrics.Memory = warnOnError(c, "memory", func() (wire.Memory, error) { return ReadMemory(c.root) })
	metrics.OOMKillsTotal = warnOnError(c, "oom kills", func() (uint64, error) { return ReadOOMKills(c.root) })

	wg.Wait()
	return host, metrics
}

// warnOnError returns read's value, or the zero value plus a warning when it fails.
func warnOnError[T any](c *Collector, source string, read func() (T, error)) T {
	value, err := read()
	if err != nil {
		c.logger.Warn("collect failed", "source", source, "err", err)
		var zero T
		return zero
	}
	return value
}

// filesystems is always an array: an unreadable mount table yields [].
func (c *Collector) filesystems() []wire.Filesystem {
	table, err := ReadMountTable(c.root)
	if err != nil {
		c.logger.Warn("collect failed", "source", "filesystems", "err", err)
		return []wire.Filesystem{}
	}
	return c.filesystem.Collect(table)
}

// failedUnits is null when systemd is absent and also when systemctl fails:
// null means "unknown", which is honest where [] would claim no failures.
func (c *Collector) failedUnits(ctx context.Context) []string {
	units, err := FailedUnits(ctx, c.root, c.runCommand)
	if err != nil {
		c.logger.Warn("collect failed", "source", "failed units", "err", err)
		return nil
	}
	return units
}

// containers is null when the Docker socket is absent or does not answer.
func (c *Collector) containers(ctx context.Context) []wire.Container {
	containers, err := c.docker.Containers(ctx)
	if err != nil {
		c.logger.Warn("collect failed", "source", "containers", "err", err)
		return nil
	}
	return containers
}
