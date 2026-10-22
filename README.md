# lookout

lookout is a small host monitoring agent. It runs on a Debian/Ubuntu server or an Unraid box, collects host metrics and runs the checks the platform asks for, then reports the results to the media-manager platform over HTTPS.

## Install on Debian/Ubuntu

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
