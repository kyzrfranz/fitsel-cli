# Epic: Garmin Sync — fitsel activity intake (v1)

## Goal

A small command-line tool the athlete runs on their own machine, by hand or on a schedule. Each run it uses the athlete's Garmin Personal API key to fetch activities they have not synced yet as FIT files and uploads each one to the athlete's fitsel upload webhook. It is a client the athlete runs — not a hosted service — because Garmin's Personal API is "not intended for commercial applications serving multiple end users": the athlete's Garmin key must never leave the athlete's machine.

v1 is "every activity since a start date, into fitsel, reliably and repeatably," proven against fakes with no network and shipped as CI-built binaries for the founder to tag.

## v1 capabilities (from the pitch's "Must")

- Authenticate against the athlete's Garmin Personal API key: fetch an access token; tokens last 24h and there are no refresh tokens, so a fresh token is requested whenever one is needed (never cached across runs past its life).
- List activities newer than the last successful sync and download each as an original FIT file.
- Upload each FIT file to fitsel per the given contract (multipart `fit` part; `X-RunGap-HMAC-SHA256` of the raw body keyed with the passkey; `202` accepted), recording progress only after fitsel answers `202`.
- Remember progress in a local state file so the next run resumes exactly where the last stopped, including after a crash midway.
- On first run, sync from a start date the athlete gives (`--since YYYY-MM-DD`).
- `--dry-run`: list what would be uploaded, upload nothing.
- Clear exit codes and one-line messages for: bad or expired Garmin key (one message is fine if Garmin's errors don't tell them apart); Garmin rate limit (back off, then stop cleanly); fitsel `400` / `401` / `404`; fitsel `500` or no network (retry a few times, then stop with its own exit code, nothing marked synced).
- A README an athlete can follow alone: create the Garmin key, get the fitsel token and passkey, configure, run, schedule.

The fitsel upload contract is given and fixed — this product adapts to it; do not change it.

## Out of scope (excluded)

Any hosted or multi-user service; storing any athlete's Garmin key anywhere but on their own machine. Garmin's Business/Developer Program (push delivery, OAuth apps). Health/sleep/women's-health data and Courses & Training. A GUI or any change to fitsel itself. Publishing the release: the team builds the binaries; the founder tags and publishes. Nothing in this epic touches those.

## First stories (build order)

Each story is one step the athlete can see, through all layers, and each is verified with tests that run with no network access (fake Garmin + a fake fitsel endpoint that checks the multipart body and HMAC signature). The team decides how the fake Garmin server follows Garmin's API description; every story below assumes that fakes exist.

### 1. First sync end-to-end (walking skeleton)
Authenticate, list activities since `--since`, download each FIT, upload via the contract to a configurable endpoint, and record per-activity progress in a state file. This is the thinnest path through all layers and gives us a testable build early.
Checks: against fakes, a first run uploads every activity since `--since`; a second run straight away uploads nothing (state makes it idempotent). CI builds at least one platform and runs the tests.

### 2. Resume after an interrupted sync
State records per-activity progress durably mid-run so an interrupted run resumes cleanly on the next run; re-uploading a file already sent is safe (fitsel dedupes by content).
Checks: a run over several activities killed after the first few completes on rerun with no errors and every activity present exactly once (no duplicates in fitsel).

### 3. Dry-run preview
`--dry-run` lists what would be uploaded without uploading anything and leaves state unchanged.
Checks: under `--dry-run` the endpoint receives nothing; output names each pending activity.

### 4. Garmin transport errors
Clear exit code + one-line message for a bad or expired key; request a new token when needed; on Garmin rate limit (`429`) back off — honour `Retry-After`, and stop cleanly. Where Garmin's docs are silent (history depth, rate limits, error bodies) choose a safe default and write it in the README.
Checks: each of these cases returns its own exit code and message, verified against fakes.

### 5. fitsel upload errors
Distinct exit codes + messages for `400` (invalid upload), `401` (bad signature), `404` (unknown webhook token); on `500` or no network retry a few times then stop with its own exit code and mark nothing synced.
Checks: each of these cases returns its own exit code and message, verified against fakes.

### 6. Config and secret hygiene
Load the Garmin key and the fitsel token + passkey from environment variables or a config file the tool creates readable only by the athlete (mode `0600` on macOS and Linux). Never log or print any key.
Checks: missing/unreadable credentials give a clear error; the config file is created with mode `0600`; no secret appears in any output or state.

### 7. Multi-platform release build
CI builds binaries for macOS (arm64), Linux (amd64 + arm64) and Windows (amd64) on every merge to `main`, publishing them as a release ready for the founder to tag (extend the existing CI by pull request, per ADR 0045).
Checks: a merge to `main` produces all four builds; the README documents how to build a release from them.

### 8. README setup guide
Steps an athlete can follow alone: create the Garmin key, get the fitsel token and passkey, configure, run (including `--since` and `--dry-run`), and schedule on each OS (documented recipes, no installer). Assumptions made where Garmin's docs are silent are written in.
Checks: all five steps are covered; the error cases and their exit codes are documented.

## Team decides (left for the architect / team)

Command and flag names beyond `--since` and `--dry-run`; config format, state-file format and location; per-OS scheduling recipes; package layout; testing approach; CI build setup; and how the fake Garmin server follows Garmin's API description. Implementation details go in each story's design, not here.

## Precondition

The fake Garmin depends on Garmin's public Personal API description being committed to this repo with notes on what it leaves out (the founder's "Decided already"). That is fetched by a Researcher desk first, or provided by the founder if the Researcher cannot reach developer.garmin.com — so story work should not stall waiting for network access to Garmin.
