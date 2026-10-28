# garmin-sync

## Configure

The tool needs three secrets, loaded from the environment first and a config
file second — an environment variable wins over the file for each key:

| Variable | Used for |
|---|---|
| `GARMIN_API_KEY` | Your Garmin Personal API key (create one at developer.garmin.com). |
| `FITSEL_TOKEN` | Your fitsel upload token (from fitsel's setup page). |
| `FITSEL_PASSKEY` | Your fitsel upload passkey (from fitsel's setup page). |

If any are missing, the tool writes a sample config file at the path below so
you know where to put them. Secrets are never logged or printed.

The config file lives at `~/.config/garmin-sync/config.json` (on Linux and
macOS), mode `0600` (readable only by you); its directory is mode `0700`. Set
`FITSEL_CONFIG` to use a different file. Edit it by hand:

```json
{"garmin_api_key":"<create one at developer.garmin.com>",
"fitsel_token":"<from fitsel's setup page>",
"fitsel_passkey":"<from fitsel's setup page>"}
```

If the file exists but is unreadable or not valid JSON, the tool reports a
clear error naming the file — fix it, and the run will proceed.

## Run

```sh
garmin-sync --since 2026-01-01
```

The first run needs `--since` to set your start date. Each later run uploads
everything pending since then. See "Exit codes" below for what a run reports.

## Schedule

Run it on a schedule with cron (macOS) or systemd timer (Linux). A run exits
`0` when it synced everything, or a non-zero code (see below) when it could
not — so a cron can tell you success from a problem. Run it daily after you've
trained.

## Exit codes

A run stops with one of these codes so an athlete (or a cron) can act on it:

| Code | Meaning | Do |
|---|---|---|
| 0 | Success. Synced everything that was pending. | Nothing. |
| 1 | A general failure (network, fitsel, state, first run without `--since`). | See the message on the `error:` line. |
| 3 | Your Garmin API key is invalid (Garmin returned 401). | Check `GARMIN_API_KEY` or create a new one at developer.garmin.com. |
| 4 | A fit file would not be accepted by fitsel (returned 400). | Check that the file is a valid FIT — nothing was marked synced, so it will be retried. |
| 5 | Garmin is rate-limiting you (429); the tool waited for the window in the `error:` line, then stopped. | Wait that long, then run again — do not retry sooner. |
| 6 | Your fitsel passkey is invalid (fitsel returned 401). | Check `FITSEL_PASSKEY` or copy the right one from fitsel's setup page. |
| 7 | The fitsel webhook token in the endpoint URL is unknown (fitsel returned 404). | Check the fitsel webhook URL you configured. |
| 8 | fitsel would not accept the upload (server error 500 or no network); the tool tried a few times, then stopped. | Wait a moment, then run again — nothing was marked synced, so the pending activity will be retried. |

More codes are added as features land (uploads). This list grows with them.

## Releases

A binary for every supported platform is produced automatically on every merge
to `main`, as this repo's CI (`.forgejo/workflows/ci.yml`). Each merge
cross-compiles all four targets and pushes them to the `release-binaries`
branch, untagged, ready to go:

| File | Platform |
|---|---|
| `garmin-sync-darwin-arm64` | macOS, Apple Silicon (M1/M2/M3/M4) |
| `garmin-sync-linux-amd64` | Linux, 64-bit x86 |
| `garmin-sync-linux-arm64` | Linux, 64-bit ARM (e.g. Raspberry Pi) |
| `garmin-sync-windows-amd64.exe` | Windows, 64-bit x86 |

To get one: check out the `release-binaries` branch, take the file for your
platform, rename it `garmin-sync` (drop the platform suffix, and the `.exe` on
Windows), make it executable (`chmod +x garmin-sync` on Linux/macOS), and run
it as under "Run".

The binaries are cross-compiled from a single Go source tree with the standard
library cross-compiler (`CGO_ENABLED=0 GOOS=… GOARCH=… go build`), so there is
no third-party toolchain to install.

Build one yourself, to test without merging, from a checkout of `main`:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o garmin-sync-linux-amd64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o garmin-sync-linux-arm64 .
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o garmin-sync-darwin-arm64 .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o garmin-sync-windows-amd64.exe .
```

The founder chooses the licence and tags/publishes v1 from these staged binaries;
CI never creates a git tag or a release.
