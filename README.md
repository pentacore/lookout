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
