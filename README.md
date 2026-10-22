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
