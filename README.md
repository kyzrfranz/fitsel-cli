# fitsel-client

A client the athlete runs on their own machine. Each run it uses the athlete's
Garmin Personal API key to fetch the activities they have not synced yet as FIT
files, and uploads each one to the athlete's fitsel upload webhook.

It is a client, not a hosted service: the athlete's Garmin key never leaves the
athlete's machine (Garmin's Personal API is "not intended for commercial
applications serving multiple end users"). That is also why there is no server to
run and no refresh token to manage — the key stays on the athlete's machine.

## Setup

Follow these six steps in order:

1. [Install](#1-install)
2. [Create the Garmin key](#2-create-the-garmin-key)
3. [Get the fitsel token and passkey](#3-get-the-fitsel-token-and-passkey)
4. [Configure](#configure)
5. [Run](#run)
6. [Schedule on each OS](#schedule)

## 1. Install

### Homebrew (macOS and Linux)

```sh
brew install kyzrfranz/tap/fitsel-client
fitsel-client --version
```

Upgrade to a newer release with `brew upgrade fitsel-client`.

### Download a release

Every release is published at
<https://github.com/kyzrfranz/fitsel-cli/releases> with an archive for each
platform (macOS, Linux and Windows; amd64 and arm64) and a `checksums.txt`.
Download the one for your platform, unpack it, and put `fitsel-client` (or
`fitsel-client.exe` on Windows) somewhere on your `PATH`.

### Build from source

```sh
go build -o fitsel-client .
```

## 2. Create the Garmin key

The tool needs a Garmin Personal API key. Create one here:

1. Go to <https://developer.garmin.com/> and sign in with your Garmin account.
2. **My Apps** → **Create App**. Give it a name, e.g. `fitsel-client`.
3. On the app's page, under **Personal API Keys**, click **Create Personal API Key**.
   A `client_id` (the key) and a `client_secret` are generated.
4. Copy the **client id** — that is the key the tool needs. Paste it into
   `GARMIN_API_KEY` (see [Configure](#configure)).

The key lasts about 24 hours. It never needs a refresh token *because* it stays on
the athlete's machine — there is no server to hold one. When it expires, just
create a new one at the same page and paste it in.

## 3. Get the fitsel token and passkey

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
| `GARMIN_API_KEY` | Your Garmin Personal API key (see [Create the Garmin key](#2-create-the-garmin-key)). |
| `FITSEL_TOKEN` | Your fitsel upload token (see [Get the fitsel token and passkey](#3-get-the-fitsel-token-and-passkey)). |
| `FITSEL_PASSKEY` | Your fitsel upload passkey (see [Get the fitsel token and passkey](#3-get-the-fitsel-token-and-passkey)). |

If any are missing, the tool writes a sample config file at the path below so
you know where to put them. Secrets are never logged or printed.

The config file lives at `~/.config/garmin-sync/config.json` (on Linux and
macOS; the directory keeps the tool's earlier name), mode `0600` (readable only by you); its directory is mode `0700`. Set
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
fitsel-client --since 2026-01-01
```

The first run needs `--since` to set your start date. Each later run uploads
everything pending since then. See "Exit codes" below for what a run reports.

A run prints a one-line summary at the end — how many activities synced and how
many failed this run — so a schedule can tell success from a partial one:

```
done: 2 synced, 1 failed this run
```

### Try it first with --dry-run

Before your first real run, preview what would be uploaded with `--dry-run`.
It lists the pending activities and uploads nothing — no files are downloaded
from Garmin, nothing is sent to fitsel, and nothing is marked synced:

```sh
fitsel-client --since 2026-01-01 --dry-run
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

- `--since <YYYY-MM-DD>` — only sync activities on or after this date. The first
  run needs it to set your start date; later runs keep it.
- `--to <YYYY-MM-DD>` — only sync activities on or before this date. Combined
  with `--since` it restricts a sync to the window `[--since, --to]`; omit it for
  the open-ended upper bound (everything since `--since`). The date is forwarded
  to Garmin's list API, so a narrow window is listed in full and never truncated
  at the old 100-activity cap.
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
0 20 * * * /opt/homebrew/bin/fitsel-client --since 2026-01-01 >> ~/.config/garmin-sync/sync.log 2>&1
```

Use the full path to the binary, since cron does not load your shell's `PATH`.
`which fitsel-client` prints it: `/opt/homebrew/bin/fitsel-client` for Homebrew
on Apple Silicon, `/usr/local/bin/fitsel-client` on Intel Macs,
`/home/linuxbrew/.linuxbrew/bin/fitsel-client` for Homebrew on Linux.

Or a systemd timer on Linux:

```ini
# ~/.config/systemd/user/fitsel-client.service
[Unit]
Description=fitsel-client

[Service]
Type=oneshot
ExecStart=/home/linuxbrew/.linuxbrew/bin/fitsel-client --since 2026-01-01
```

```ini
# ~/.config/systemd/user/fitsel-client.timer
[Unit]
Description=fitsel-client daily

[Timer]
OnCalendar=*-*-* 20:00:00
Persistent=true

[Install]
WantedBy=default.target
```

```sh
systemctl --user enable --now fitsel-client.timer
```

### Windows (Task Scheduler)

On Windows, create a daily task in Task Scheduler. From a command prompt:

```bat
schtasks /create /tn "fitsel-client" /tr "C:\path\to\fitsel-client.exe --since 2026-01-01" /sc daily /st 20:00
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
| 9 | One or more activities failed to sync (a single bad upload or a download that dropped), but the rest of the run completed. | The other activities were synced and marked done; the failed ones are still pending. Run again to retry them — nothing marked synced needs to be retried. |

More codes are added as features land. This list grows with them.

## Releases

Releases are published from the public GitHub mirror,
[kyzrfranz/fitsel-cli](https://github.com/kyzrfranz/fitsel-cli). Pushing a `v*`
tag there runs `.github/workflows/release.yml`, which runs the tests and then
[GoReleaser](https://goreleaser.com) (`.goreleaser.yaml`) to:

- build `fitsel-client` for macOS, Linux and Windows (amd64 and arm64),
- publish them as a GitHub Release with a `checksums.txt`,
- update the `fitsel-client` cask in
  [kyzrfranz/homebrew-tap](https://github.com/kyzrfranz/homebrew-tap).

To cut a release, from an up-to-date `main`:

```sh
git push public main
git tag -a v0.1.0 -m "v0.1.0"
git push public v0.1.0
```

The workflow needs the `HOMEBREW_TAP_TOKEN` secret on `kyzrfranz/fitsel-cli`: a
fine-grained token with Contents read/write on `kyzrfranz/homebrew-tap`.

Try a release locally without publishing anything:

```sh
goreleaser release --snapshot --clean   # output in dist/
```

### Internal builds

Separately, the Forgejo CI (`.forgejo/workflows/ci.yml`) cross-compiles a binary
on every merge to `main` and pushes it to the `release-binaries` branch,
untagged. Those files still carry the old `garmin-sync-*` names; rename the one
for your platform to `fitsel-client` before use.
