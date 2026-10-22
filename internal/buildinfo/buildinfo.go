// Package buildinfo holds values stamped into the binary at release time.
package buildinfo

// Version is the release version. Release builds override it with
// -ldflags "-X github.com/pentacore/lookout/internal/buildinfo.Version=...".
var Version = "dev"

// MinisignPublicKey is the base64 minisign public key used to verify
// self-updates. It is empty on development builds, which disables updates.
var MinisignPublicKey = ""

// UserAgent returns the User-Agent header value sent on every request.
func UserAgent() string {
	return "lookout/" + Version
}
