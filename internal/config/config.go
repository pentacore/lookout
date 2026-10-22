// Package config loads and edits the agent's local YAML configuration.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Platform is the kind of host the agent runs on. It selects default paths.
type Platform string

const (
	PlatformSystemd Platform = "systemd"
	PlatformUnraid  Platform = "unraid"
)

const (
	keyPlatformURL = "platform_url"
	keyStateDir    = "state_dir"
	keyRuntimeDir  = "runtime_dir"
	keyAutoUpdate  = "auto_update"

	runtimeDir = "/run/lookout"
)

var knownKeys = map[string]bool{
	keyPlatformURL: true,
	keyStateDir:    true,
	keyRuntimeDir:  true,
	keyAutoUpdate:  true,
}

// DetectPlatform reports PlatformUnraid when {root}/etc/unraid-version exists
// and PlatformSystemd otherwise.
func DetectPlatform(root string) Platform {
	if _, err := os.Stat(filepath.Join(root, "etc", "unraid-version")); err == nil {
		return PlatformUnraid
	}
	return PlatformSystemd
}

// DefaultPath returns the config file location for the platform.
func DefaultPath(p Platform) string {
	if p == PlatformUnraid {
		return "/boot/config/plugins/lookout/lookout.yaml"
	}
	return "/etc/lookout/lookout.yaml"
}

func defaultStateDir(p Platform) string {
	if p == PlatformUnraid {
		return "/boot/config/plugins/lookout/state"
	}
	return "/var/lib/lookout"
}

// Config is the agent's local configuration. Phase 3/4 adds the `actions` and
// `reflexes` sections here.
type Config struct {
	PlatformURL string `yaml:"platform_url"`
	StateDir    string `yaml:"state_dir"`
	RuntimeDir  string `yaml:"runtime_dir"`
	AutoUpdate  bool   `yaml:"auto_update"`

	// Platform is the detected host kind; Path is the file the config was
	// loaded from; UnknownKeys lists top-level keys that are not recognised.
	Platform    Platform `yaml:"-"`
	Path        string   `yaml:"-"`
	UnknownKeys []string `yaml:"-"`
}

// fileConfig is the on-disk shape. AutoUpdate is a pointer so that an absent
// key can be told apart from an explicit false.
type fileConfig struct {
	PlatformURL string `yaml:"platform_url"`
	StateDir    string `yaml:"state_dir"`
	RuntimeDir  string `yaml:"runtime_dir"`
	AutoUpdate  *bool  `yaml:"auto_update"`
}

// Load reads the config at path, filling defaults for platform p first. A
// missing file returns the defaults together with an error wrapping
// fs.ErrNotExist. Unknown top-level keys are collected, not rejected.
func Load(path string, p Platform) (Config, error) {
	cfg := Config{
		StateDir:   defaultStateDir(p),
		RuntimeDir: runtimeDir,
		AutoUpdate: true,
		Platform:   p,
		Path:       path,
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Kind == 0 {
		return cfg, nil
	}
	var raw fileConfig
	if err := doc.Decode(&raw); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}

	cfg.PlatformURL = raw.PlatformURL
	if raw.StateDir != "" {
		cfg.StateDir = raw.StateDir
	}
	if raw.RuntimeDir != "" {
		cfg.RuntimeDir = raw.RuntimeDir
	}
	if raw.AutoUpdate != nil {
		cfg.AutoUpdate = *raw.AutoUpdate
	}

	if root := doc.Content[0]; root.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(root.Content); i += 2 {
			if key := root.Content[i].Value; !knownKeys[key] {
				cfg.UnknownKeys = append(cfg.UnknownKeys, key)
			}
		}
	}
	return cfg, nil
}

// Validate checks that platform_url is set, has a host and uses https. Plain
// http is accepted for loopback hosts only.
func (c Config) Validate() error {
	if c.PlatformURL == "" {
		return errors.New("platform_url is required")
	}
	u, err := url.Parse(c.PlatformURL)
	if err != nil {
		return fmt.Errorf("platform_url is not a valid URL: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("platform_url has no host")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
	}
	return errors.New("platform_url must use https (http is allowed only for localhost, 127.0.0.1 and ::1)")
}

// BaseURL returns platform_url without trailing slashes.
func (c Config) BaseURL() string {
	return strings.TrimRight(c.PlatformURL, "/")
}

// SetPlatformURL writes platform_url into the config at path. A missing file
// is created containing only that key; otherwise the value is edited through
// a yaml.Node round trip so other keys and comments survive.
func SetPlatformURL(path, platformURL string) error {
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var parsed yaml.Node
		if err := yaml.Unmarshal(data, &parsed); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		if parsed.Kind != 0 {
			doc = &parsed
		}
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("create config dir: %w", err)
		}
	default:
		return fmt.Errorf("read config: %w", err)
	}

	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("parse %s: top level is not a mapping", path)
	}
	setScalar(root, keyPlatformURL, platformURL)

	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// setScalar sets key to a string value in the mapping node, appending the
// pair when the key is absent.
func setScalar(mapping *yaml.Node, key, value string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			v := mapping.Content[i+1]
			v.Kind, v.Tag, v.Style, v.Value = yaml.ScalarNode, "!!str", 0, value
			v.Content, v.Alias = nil, nil
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}
