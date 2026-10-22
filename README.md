# lookout

lookout is a small host monitoring agent. It runs on a Debian/Ubuntu server or an Unraid box, collects host metrics and runs the checks the platform asks for, then reports the results to the media-manager platform over HTTPS.

## Install on Debian/Ubuntu

The install script supports `amd64` and `arm64` systems that run systemd. Create an enrollment token on the platform, then run:

```sh
curl --proto '=https' --tlsv1.2 -fsSLO https://github.com/pentacore/lookout/releases/latest/download/install.sh
sudo sh install.sh --url https://media.pentacore.se --token <TOKEN>
```

The script downloads the release archive, verifies it, installs `/usr/local/bin/lookout`, writes `/etc/lookout/lookout.yaml`, enrolls the server, and enables and starts the `lookout` systemd service. Options:

| Flag | Meaning |
| --- | --- |
| `--url` | Platform base URL. Required on first install. |
| `--token` | Enrollment token. Required until the server is enrolled. |
| `--version` | Release to install, such as `0.2.0` or `latest`. Defaults to the version the script was released with. |

Running the script again is safe. It upgrades the binary, keeps `/etc/lookout/lookout.yaml` and restarts the service. With no `--url` it reuses the `platform_url` from the config. Passing `--token` on an enrolled server re-enrolls it. Existing credentials are never pointed at a different platform: a different `--url` on an enrolled server is refused unless you also pass `--token`, and then the server is re-enrolled with the new platform (`lookout enroll --force` updates `platform_url`).

The enrollment token is passed as a command-line argument, so it is visible in the process list while the script runs. This is accepted for now because the token is single-use, expires after 24 hours and the host is root-only. The script never prints it.

### How the download is verified

If `minisign` is installed, the script checks the archive's signature against the public key built into the script. Otherwise it checks the archive against `checksums.txt` and prints a warning. Both checks trust the same GitHub release that served the script. A minisign check only adds authenticity when you obtained the public key out of band, for example by comparing it with the key published in the repository. Every later self-update is verified with minisign by the agent itself.

## Install on Unraid

The plugin needs Unraid 6.12 or later.

1. Go to **Plugins → Install Plugin**, paste `https://github.com/pentacore/lookout/releases/latest/download/lookout.plg` and click **Install**.
2. Create an enrollment token on the platform.
3. Go to **Settings → User Utilities → Lookout**, enter the platform URL and the token, and click **Enroll**.

The page shows whether the agent runs, its version, the server UUID, the platform URL, and the time and error of the last report. Enrolling again from the page re-enrolls the server (`lookout enroll --force`) and restarts the agent. The enrollment token is passed to `lookout enroll` as a command-line argument, so it shows in the process list while enrolling. It is never written to the log or shown on the page.

The plugin starts the agent at boot, not at array start, so it also runs while the array is stopped. A small supervisor restarts the agent 5 seconds after it exits. Use `/etc/rc.d/rc.lookout start|stop|restart|status` to control it by hand.

| What | Where |
| --- | --- |
| Config | `/boot/config/plugins/lookout/lookout.yaml` |
| Credentials and applied platform config | `/boot/config/plugins/lookout/state/` |
| Binary (persistent copy) | `/boot/config/plugins/lookout/lookout`, copied to `/usr/local/bin/lookout` at install and at every boot, because `/boot` is not executable |
| Status file | `/run/lookout/status.json` |
| Logs | `/var/log/syslog`, tag `lookout` |

The plugin downloads the agent from the GitHub release and checks it against `checksums.txt` (Unraid has no `minisign`), only when the persistent copy is missing or older than the plugin's release. Self-updates are verified with minisign by the agent and also update the persistent copy.

Removing the plugin stops the agent and removes the binary, the rc script and the settings page. It keeps `/boot/config/plugins/lookout` (config, credentials and binary), so a reinstall keeps the server's identity. Delete that folder to forget the server.

## Configuration

The agent reads a YAML file: `/etc/lookout/lookout.yaml` on Debian/Ubuntu and `/boot/config/plugins/lookout/lookout.yaml` on Unraid. All keys are optional except `platform_url`.

| Key | Default (Debian/Ubuntu) | Default (Unraid) | Meaning |
| --- | --- | --- | --- |
| `platform_url` | none (required) | none (required) | Base URL of the media-manager platform. |
| `state_dir` | `/var/lib/lookout` | `/boot/config/plugins/lookout/state` | Where credentials and other state are kept. |
| `runtime_dir` | `/run/lookout` | `/run/lookout` | Volatile runtime files, such as the status file. |
| `auto_update` | `true` | `true` | Download, verify and install new releases automatically. |

`platform_url` must use `https://`. Plain `http://` is accepted only for loopback hosts (`localhost`, `127.0.0.1`, `::1`), which is meant for local development.

Unknown top-level keys, such as a misspelled `platfrom_url`, do not stop the agent. They are logged as a warning and ignored.

## Releases and signing

Releases are built by GoReleaser in the `Release` workflow (`.github/workflows/release.yml`) when a `v*` tag is pushed. Each release has these assets:

| Asset | What it is |
| --- | --- |
| `lookout_linux_amd64.tar.gz`, `lookout_linux_arm64.tar.gz` | The static `lookout` binary, alone in the archive. |
| `lookout_linux_{amd64,arm64}.tar.gz.minisig` | The minisign signature of each archive. |
| `checksums.txt` | SHA-256 of both archives. |
| `install.sh`, `lookout.plg` | The Debian/Ubuntu install script and the Unraid plugin, with the version and public key filled in. |

### One-time setup (repository owner)

1. Create the **public** GitHub repo `pentacore/lookout`. It must be public because agents call the GitHub releases API and download assets without authentication. Add a LICENSE of your choice.
2. Generate the signing key: `go install aead.dev/minisign/cmd/minisign@v0.3.0 && minisign -G -p lookout.pub -s lookout.key` (enter a password).
3. Add repo **secrets** `MINISIGN_SECRET_KEY` (full contents of `lookout.key`) and `MINISIGN_PASSWORD`. Add repo **variable** `MINISIGN_PUBLIC_KEY` (the second line of `lookout.pub`, the base64 `RW…` string). Store `lookout.key` offline.
4. Add the remote, push `main`, then tag `v0.1.0` and push the tag to cut the first release (see below).
5. Optional: put the public key in the repo description or this README, so first installs can be checked out of band.
6. Recommended: add a tag ruleset for `v*` so only you can create, move or delete release tags. For a second gate, move the two secrets into a `release` environment with required reviewers and add `environment: release` to the job in `release.yml`.

### Cutting a release

```sh
git tag vX.Y.Z && git push --tags
```

The tag must be `vX.Y.Z` or `vX.Y.Z-prerelease` (such as `v0.2.0-rc.1`). A tag with a pre-release suffix becomes a GitHub pre-release, which agents ignore because they only look at the latest full release. The workflow:

1. Refuses any other tag format.
2. Signs a test file with the secret key and password, and verifies it with `MINISIGN_PUBLIC_KEY`. If the secrets and the variable don't belong together, the release stops here, before anything is built or published.
3. Renders `install.sh` and `lookout.plg`, then builds, signs and uploads the release with GoReleaser as a **draft**. The public key and the version are built into the binary. Agents and `install.sh` can't see a draft.
4. Verifies the archives exactly as uploaded with `scripts/verify-release-dist.sh`: the minisign signature and exact trusted comment of each archive, `checksums.txt`, that each archive holds only `lookout`, that both binaries embed `MINISIGN_PUBLIC_KEY`, and the agent's own verification code (`go test -tags release` in `internal/selfupdate`).
5. Publishes the draft only when all of that passes. If a check fails, the draft stays unpublished: inspect it, delete it on GitHub, fix the problem and push a new tag (or delete and re-push the same one).

CI runs the same build and pre-publish check on every push, with a throwaway key and a snapshot build. To run it locally (needs Go 1.27.1 or later, or set `GORELEASER=goreleaser` to use an installed goreleaser; it rewrites `dist/` and `build/release-extra/`):

```sh
bash packaging/test/release_test.sh
```

### What the agent verifies

When a newer release than its own version is out, the agent downloads the archive for its architecture and its `.minisig` and checks:

- The signature is valid for the public key built into the running binary.
- The signature's trusted comment is exactly `lookout {version} {asset}`, for example `lookout 0.2.0 lookout_linux_amd64.tar.gz`. The trusted comment is covered by the signature, but the release tag is not. So this check is what stops an attacker who controls the download from serving an older signed release (a downgrade) or the other architecture's archive under this release's name.
- The archive holds exactly one `lookout` entry, a regular file, and the new binary reports the expected version when run with `lookout version`.

A build without an embedded public key, such as a local `go build` or a `dev` build, never self-updates.
