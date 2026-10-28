# garmin-sync

A client the athlete runs on their own machine. Each run it uses the athlete's
Garmin Personal API key to fetch the activities they have not synced yet as FIT
files, and uploads each one to the athlete's fitsel upload webhook.

It is a client, not a hosted service: the athlete's Garmin key never leaves the
athlete's machine (Garmin's Personal API is "not intended for commercial
applications serving multiple end users"). That is also why there is no server to
run and no refresh token to manage — the key stays on the athlete's machine.

## Setup

Follow these five steps in order:

1. [Create the Garmin key](#1-create-the-garmin-key)
2. [Get the fitsel token and passkey](#2-get-the-fitsel-token-and-passkey)
3. [Configure](#configure)
4. [Run](#run)
5. [Schedule on each OS](#schedule)

## 1. Create the Garmin key

The tool needs a Garmin Personal API key. Create one here:

1. Go to <https://developer.garmin.com/> and sign in with your Garmin account.
2. **My Apps** → **Create App**. Give it a name, e.g. `garmin-sync`.
3. On the app's page, under **Personal API Keys**, click **Create Personal API Key**.
   A `client_id` (the key) and a `client_secret` are generated.
4. Copy the **client id** — that is the key the tool needs. Paste it into
   `GARMIN_API_KEY` (see [Configure](#configure)).

The key lasts about 24 hours. It never needs a refresh token *because* it stays on
the athlete's machine — there is no server to hold one. When it expires, just
create a new one at the same page and paste it in.

## 2. Get the fitsel token and passkey

The tool uploads to your fitsel upload webhook. Get the credentials for it here:

1. Sign in to fitsel and open your **Upload Webhook** page
   (<https://fitsel.app/settings/uploads> — the webhook for your account).
2. Copy the **token** and the **passkey** shown there. They are the two secrets
   the tool needs from fitsel.
3. Paste the token into `FITSEL_TOKEN` and the passkey into `FITSEL_PASSKEY`
   (see [Configure](#configure)).

The tool sends uploads to `https://ingest.fitsel.app/ingest/wh` by default; your
token and passkey are embedded in the webhook URL you copied, so you do not need
to touch that URL unless you want to override it (see `--fitsel-endpoint`).

## Configure

The tool needs three secrets, loaded from the environment first and a config
file second — an environment variable wins over the file for each key:

| Variable | Used for |
|---|---|
| `GARMIN_API_KEY` | Your Garmin Personal API key (see [Create the Garmin key](#1-create-the-garmin-key)). |
| `FITSEL_TOKEN` | Your fitsel upload token (see [Get the fitsel token and passkey](#2-get-the-fitsel-token-and-passkey)). |
| `FITSEL_PASSKEY` | Your fitsel upload passkey (see [Get the fitsel token and passkey](#2-get-the-fitsel-token-and-passkey)). |

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

### Try it first with --dry-run

Before your first real run, preview what would be uploaded with `--dry-run`.
It lists the pending activities and uploads nothing — no files are downloaded
from Garmin, nothing is sent to fitsel, and nothing is marked synced:

```sh
garmin-sync --since 2026-01-01 --dry-run
```

It prints something like:

```
3 activity(ies) would be uploaded:
  - Morning Run (1001)
  - Evening Ride (1002)
  - Weekend Hike (1003)
```

When the list looks right, run it again without `--dry-run` to upload.

### Optional flags

- `--state <path>` — where to keep the state file (which activities are synced).
  Default: `~/.config/garmin-sync/state.json`.
- `--fitsel-endpoint <url>` — the fitsel webhook base URL. Default:
  `https://ingest.fitsel.app/ingest/wh`.

## Schedule

Run it on a schedule with cron (macOS) or systemd timer (Linux) or Task Scheduler
(Windows). A run exits `0` when it synced everything, or a non-zero code (see
below) when it could not — so a schedule can tell you success from a problem. Run
it daily after you've trained.

### macOS and Linux (cron / systemd)

Add a cron entry (macOS and Linux alike) to run daily after training:

```sh
crontab -e
```

```
0 20 * * * /usr/local/bin/garmin-sync --since 2026-01-01 >> ~/.config/garmin-sync/sync.log 2>&1
```

Or a systemd timer on Linux:

```ini
# ~/.config/systemd/user/garmin-sync.service
[Unit]
Description=garmin-sync

[Service]
Type=oneshot
ExecStart=/usr/local/bin/garmin-sync --since 2026-01-01
```

```ini
# ~/.config/systemd/user/garmin-sync.timer
[Unit]
Description=garmin-sync daily

[Timer]
OnCalendar=*-*-* 20:00:00
Persistent=true

[Install]
WantedBy=default.target
```

```sh
systemctl --user enable --now garmin-sync.timer
```

### Windows (Task Scheduler)

On Windows, create a daily task in Task Scheduler. From a command prompt:

```bat
schtasks /create /tn "garmin-sync" /tr "C:\path\to\garmin-sync-windows-amd64.exe --since 2026-01-01" /sc daily /st 20:00
```

Run it daily after you've trained. A run exits `0` on success, or a non-zero code
(see below) on a problem.

## Exit codes

A run stops with one of these codes so an athlete (or a schedule) can act on it:

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
