// Package selfupdate replaces the running lookout binary with the latest
// GitHub release. Nothing is written next to the executable until the
// archive's minisign signature and signed trusted comment
// ("lookout {version} {asset}") verify, and the running binary is replaced
// only after the new one has run and printed the expected version.
package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pentacore/lookout/internal/buildinfo"
)

const (
	defaultReleaseURL  = "https://api.github.com/repos/pentacore/lookout/releases/latest"
	defaultMaxDownload = 64 << 20
	maxReleaseDoc      = 2 << 20
	maxSignature       = 64 << 10
	maxBinary          = 64 << 20
	maxRedirects       = 10

	apiTimeout       = 30 * time.Second
	downloadTimeout  = 10 * time.Minute
	preflightTimeout = 5 * time.Second

	initialDelay  = 5 * time.Minute
	checkInterval = 6 * time.Hour
	checkJitter   = 30 * time.Minute

	binaryName = "lookout"
)

var (
	// ErrRestart means a new binary was installed; the caller should exit so
	// the service manager starts it.
	ErrRestart = errors.New("selfupdate: restart required")
	// ErrNoPublicKey means no minisign public key was built in, so nothing
	// can be verified and updates are disabled.
	ErrNoPublicKey = errors.New("selfupdate: no signing key built in")
	// ErrDevBuild means the running binary is a development build, which
	// never self-updates.
	ErrDevBuild = errors.New("selfupdate: development build")
)

// Updater checks GitHub for a newer release and installs it. Zero fields get
// defaults on first use: Current and PublicKey from buildinfo, Arch from
// runtime.GOARCH, Exe from the resolved os.Executable, the pentacore/lookout
// releases/latest URL, a 64 MiB download cap, an HTTP client that only
// follows https redirects, os/exec, a context-aware sleep, math/rand and a
// text logger on stderr. It is not safe for concurrent use.
type Updater struct {
	Current, PublicKey, Arch, Exe, PersistCopy, ReleaseURL string

	HTTP        *http.Client
	Exec        func(ctx context.Context, path string, args ...string) ([]byte, error)
	MaxDownload int64
	Sleep       func(context.Context, time.Duration) error
	Rand        func() float64
	Logger      *slog.Logger
}

// Loop sleeps 5 minutes, checks, then checks again every 6 h plus up to
// 30 min of jitter. Failed checks are logged at warn and retried on the next
// round. It returns ErrRestart after a successful install and nil when ctx is
// cancelled. Without a signing key, or on a "dev" build, it logs a warning
// and returns nil at once.
func (u *Updater) Loop(ctx context.Context) error {
	u.setDefaults()
	if u.PublicKey == "" {
		u.Logger.Warn("self-update disabled: no signing key built in")
		return nil
	}
	if u.Current == "dev" {
		u.Logger.Warn("self-update disabled: development build", "version", u.Current)
		return nil
	}

	delay := initialDelay
	for {
		if u.Sleep(ctx, delay) != nil {
			return nil
		}
		version, err := u.Check(ctx)
		switch {
		case err == nil && version != "":
			u.Logger.Info("self-update installed", "from", u.Current, "to", version)
			return ErrRestart
		case ctx.Err() != nil:
			return nil
		case err != nil:
			u.Logger.Warn("self-update check failed", "error", err)
		}
		delay = checkInterval + time.Duration(u.Rand()*float64(checkJitter))
	}
}

// Check installs the latest release when it is newer than Current and
// returns its version, or "" when Current is already the latest. On any
// error the installed binary is left as it was.
func (u *Updater) Check(ctx context.Context) (newVersion string, err error) {
	u.setDefaults()
	if u.PublicKey == "" {
		return "", ErrNoPublicKey
	}
	if u.Current == "dev" {
		return "", ErrDevBuild
	}
	exe, err := u.executable()
	if err != nil {
		return "", fmt.Errorf("locate executable: %w", err)
	}

	rel, err := u.latestRelease(ctx)
	if err != nil {
		return "", err
	}
	newer, err := CompareSemver(rel.TagName, u.Current)
	if err != nil {
		return "", fmt.Errorf("compare latest release with %s: %w", u.Current, err)
	}
	if newer <= 0 {
		u.Logger.Debug("self-update: already current", "version", u.Current, "latest", rel.TagName)
		return "", nil
	}
	version := strings.TrimPrefix(rel.TagName, "v")
	u.Logger.Info("self-update available", "from", u.Current, "to", version)

	archiveName := "lookout_linux_" + u.Arch + ".tar.gz"
	sigName := archiveName + ".minisig"
	archiveURL, err := rel.assetURL(archiveName)
	if err != nil {
		return "", err
	}
	sigURL, err := rel.assetURL(sigName)
	if err != nil {
		return "", err
	}
	archive, err := u.download(ctx, archiveName, archiveURL, u.MaxDownload)
	if err != nil {
		return "", err
	}
	sig, err := u.download(ctx, sigName, sigURL, maxSignature)
	if err != nil {
		return "", err
	}
	if err := VerifyArchive(u.PublicKey, archive, sig, "lookout "+version+" "+archiveName); err != nil {
		return "", fmt.Errorf("verify %s: %w", archiveName, err)
	}
	bin, err := ExtractBinary(archive, binaryName, maxBinary)
	if err != nil {
		return "", fmt.Errorf("extract %s: %w", archiveName, err)
	}
	if err := u.preflight(ctx, exe, bin, version); err != nil {
		return "", err
	}
	if err := Install(exe, bin, u.PersistCopy); err != nil {
		return "", fmt.Errorf("install %s: %w", version, err)
	}
	return version, nil
}

type release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (r release) assetURL(name string) (string, error) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL, nil
		}
	}
	return "", fmt.Errorf("asset %s not found", name)
}

func (u *Updater) latestRelease(ctx context.Context) (release, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	body, err := u.get(ctx, u.ReleaseURL, "application/vnd.github+json", maxReleaseDoc)
	if err != nil {
		return release{}, fmt.Errorf("fetch latest release: %w", err)
	}
	var rel release
	if err := json.Unmarshal(body, &rel); err != nil {
		return release{}, fmt.Errorf("decode latest release: %w", err)
	}
	return rel, nil
}

func (u *Updater) download(ctx context.Context, name, rawURL string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	data, err := u.get(ctx, rawURL, "application/octet-stream", limit)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	return data, nil
}

// get fetches an https URL and returns its body, failing on a status other
// than 200 or a body over limit bytes.
func (u *Updater) get(ctx context.Context, rawURL, accept string, limit int64) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("refusing non-https URL %q", truncate(rawURL))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", buildinfo.UserAgent())

	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	tooLarge := fmt.Errorf("response exceeds the %d byte limit", limit)
	if resp.ContentLength > limit {
		return nil, tooLarge
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, tooLarge
	}
	return data, nil
}

// preflight writes bin to {exe}.new, runs "{exe}.new version" and requires
// exactly "lookout {version}\n", so a binary that cannot start (wrong arch,
// truncated) never replaces the running one. {exe}.new is always removed;
// Install writes it again from the same verified bytes.
func (u *Updater) preflight(ctx context.Context, exe string, bin []byte, version string) error {
	tmp := exe + ".new"
	if err := writeExecutable(tmp, bin, true); err != nil {
		return fmt.Errorf("preflight failed: write %s: %w", tmp, err)
	}
	defer func() { _ = os.Remove(tmp) }()

	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	out, err := u.Exec(ctx, tmp, "version")
	if err != nil {
		return fmt.Errorf("preflight failed: %s version: %w", tmp, err)
	}
	if want := binaryName + " " + version + "\n"; string(out) != want {
		return fmt.Errorf("preflight failed: %s version printed %q, want %q", tmp, truncate(string(out)), want)
	}
	return nil
}

func (u *Updater) executable() (string, error) {
	if u.Exe != "" {
		return u.Exe, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	u.Exe = exe
	return exe, nil
}

func (u *Updater) setDefaults() {
	if u.Current == "" {
		u.Current = buildinfo.Version
	}
	if u.PublicKey == "" {
		u.PublicKey = buildinfo.MinisignPublicKey
	}
	if u.Arch == "" {
		u.Arch = runtime.GOARCH
	}
	if u.ReleaseURL == "" {
		u.ReleaseURL = defaultReleaseURL
	}
	if u.HTTP == nil {
		u.HTTP = &http.Client{CheckRedirect: httpsOnlyRedirect}
	}
	if u.Exec == nil {
		u.Exec = runCommand
	}
	if u.MaxDownload <= 0 {
		u.MaxDownload = defaultMaxDownload
	}
	if u.Sleep == nil {
		u.Sleep = sleep
	}
	if u.Rand == nil {
		u.Rand = rand.Float64
	}
	if u.Logger == nil {
		u.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
}

// httpsOnlyRedirect follows GitHub's asset redirects but never to plain http.
func httpsOnlyRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to non-https %s", req.URL.Redacted())
	}
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	return nil
}

func runCommand(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.WaitDelay = time.Second
	return cmd.Output()
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
