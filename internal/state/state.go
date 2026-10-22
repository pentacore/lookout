// Package state persists the agent's credentials and the last applied
// platform configuration. The state directory may live on a flash device, so
// writes are atomic and durable.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/pentacore/lookout/internal/wire"
)

const (
	credentialsFile    = "credentials.json"
	platformConfigFile = "platform-config.json"

	dirPerm  fs.FileMode = 0o700
	filePerm fs.FileMode = 0o600

	defaultReportIntervalSeconds = 60
)

// Credentials identify this agent to the platform.
type Credentials struct {
	ServerUUID string `json:"server_uuid"`
	AgentToken string `json:"agent_token"`
}

// PlatformConfig is the last configuration the platform handed to the agent.
type PlatformConfig struct {
	ConfigVersion         int64              `json:"config_version"`
	ReportIntervalSeconds int                `json:"report_interval_seconds"`
	Checks                []wire.CheckConfig `json:"checks"`
}

// Store reads and writes the agent's state files below Dir.
type Store struct {
	Dir string
}

// LoadCredentials returns the saved credentials. A missing file yields an
// error wrapping fs.ErrNotExist; an empty server UUID, or a token that does
// not belong to the server UUID, is an error.
func (s Store) LoadCredentials() (Credentials, error) {
	var c Credentials
	if err := s.load(credentialsFile, &c); err != nil {
		return Credentials{}, err
	}
	if c.ServerUUID == "" {
		return Credentials{}, errors.New("credentials: server_uuid is empty")
	}
	if !strings.HasPrefix(c.AgentToken, c.ServerUUID+".") {
		return Credentials{}, fmt.Errorf("credentials: agent_token does not start with server_uuid and a dot")
	}
	return c, nil
}

// SaveCredentials writes the credentials file with mode 0600, creating the
// state directory with mode 0700 when needed.
func (s Store) SaveCredentials(c Credentials) error {
	return s.save(credentialsFile, c)
}

// RemoveCredentials deletes the credentials file. A missing file is not an
// error.
func (s Store) RemoveCredentials() error {
	err := os.Remove(filepath.Join(s.Dir, credentialsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// LoadPlatformConfig returns the saved platform configuration, or the
// defaults (version 0, 60 s, no checks) when none has been saved.
func (s Store) LoadPlatformConfig() (PlatformConfig, error) {
	c := PlatformConfig{ReportIntervalSeconds: defaultReportIntervalSeconds}
	err := s.load(platformConfigFile, &c)
	if errors.Is(err, fs.ErrNotExist) {
		return PlatformConfig{ReportIntervalSeconds: defaultReportIntervalSeconds, Checks: []wire.CheckConfig{}}, nil
	}
	if err != nil {
		return PlatformConfig{}, err
	}
	if c.Checks == nil {
		c.Checks = []wire.CheckConfig{}
	}
	return c, nil
}

// SavePlatformConfig writes the platform configuration file with mode 0600.
func (s Store) SavePlatformConfig(c PlatformConfig) error {
	return s.save(platformConfigFile, c)
}

func (s Store) load(name string, v any) error {
	path := filepath.Join(s.Dir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func (s Store) save(name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	if err := os.MkdirAll(s.Dir, dirPerm); err != nil {
		return err
	}
	return WriteFileAtomic(filepath.Join(s.Dir, name), append(data, '\n'), filePerm)
}

// WriteFileAtomic replaces path with data. It writes a temporary file in the
// same directory, fsyncs it, sets perm, renames it over path and fsyncs the
// directory, so a power loss leaves either the old or the new content. Once
// the rename has happened the write counts as done: a directory fsync that
// the file system does not support is ignored and any other fsync failure is
// logged, not returned.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	// CreateTemp already made the file 0600; skipping the chmod then also
	// spares file systems with fixed modes (the vfat flash on Unraid).
	if perm != filePerm {
		if err = tmp.Chmod(perm); err != nil {
			return err
		}
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if syncErr := syncDirFunc(dir); syncErr != nil &&
		!errors.Is(syncErr, syscall.EINVAL) && !errors.Is(syncErr, errors.ErrUnsupported) {
		slog.Warn("wrote file but could not fsync its directory", "path", path, "error", syncErr)
	}
	return nil
}

// syncDirFunc is syncDir; tests replace it to simulate fsync failures.
var syncDirFunc = syncDir

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
