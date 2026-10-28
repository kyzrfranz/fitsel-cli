# Pitch: Garmin Sync                      slug: garmin-sync   version: 1   date: 2026-10-28

## Problem
fitsel (fitsel.app) analyses FIT activity files. Today athletes get their activities into fitsel through RunGap, a paid third-party app that forwards each activity to fitsel's upload webhook. Garmin has just opened a Personal API: every Garmin user can create their own API key and pull their own activities as FIT files. Athletes with a Garmin watch could feed fitsel directly, without a middleman, but no tool does that today.

## Users
- Primary: a fitsel athlete with a Garmin device, comfortable running a command-line tool or a scheduled job on their own computer.
- Secondary: the founder, as the first user and the person who accepts the release.

## The bet
A small command-line tool that the athlete runs on their own machine, by hand or on a schedule. Each run it uses the athlete's Garmin Personal API key to fetch the activities they haven't synced yet as FIT files, and uploads each one to the athlete's fitsel upload webhook. It is a client the athlete runs, not a hosted service. Garmin's Personal API is "not intended for commercial applications serving multiple end users", so the athlete's Garmin key must never leave the athlete's machine.

## First release (v1)
- Must:
  - Fetch a Garmin access token from the athlete's Personal API key (tokens last 24h; there are no refresh tokens, so request a new token whenever needed).
  - List the athlete's activities newer than the last successful sync, and download each one as an original FIT file.
  - Upload each FIT file to fitsel using the contract below, and record progress only after fitsel answers `202`.
  - Remember progress in a local state file, so the next run picks up where the last one stopped, including after a crash halfway through.
  - On first run, sync from a start date the athlete gives (`--since YYYY-MM-DD`).
  - `--dry-run`: list what would be uploaded, upload nothing.
  - Clear exit codes and one-line messages for: bad or expired Garmin key (one message is fine if Garmin's errors don't tell them apart), Garmin rate limit (back off, then stop cleanly), fitsel `400` (invalid upload), `401` (bad signature), `404` (unknown fitsel token), and fitsel `500` or no network (retry a few times, then stop with its own exit code; nothing is marked synced).
  - A README with setup steps an athlete can follow alone: create the Garmin key, get the fitsel token and passkey, configure, run, schedule.
- Done when (the team proves this; tests run without network access):
  - Against a fake Garmin server built from Garmin's public API description, and a fake fitsel endpoint that checks the multipart body and the HMAC signature, a first run uploads every activity since `--since`.
  - A second run straight away uploads nothing.
  - A run killed halfway through, then run again, finishes without errors and leaves no duplicate activities in fitsel (the one file in flight may be sent again).
  - Every error case in "Must" has a test that checks its exit code and message.
  - CI builds binaries for macOS (arm64), Linux (amd64, arm64) and Windows (amd64) on every merge to main, and the README says how to make a release from them. I tag and publish v1 myself after choosing the licence.
- Accepted when (the founder does this alone; the team does not wait for it or ask for its output):
  - I run the release with my own Garmin key and fitsel token, and my activities show up in fitsel. If something fails, I file it as a fix with what the team needs to know.

## fitsel upload contract (given; do not change)
- `POST https://ingest.fitsel.app/ingest/wh/<webhook-token>`
- Body: `multipart/form-data` with the FIT file in a part named `fit` (`file` and `activity` are also accepted). An optional `metadata` part (JSON) is logged and otherwise ignored.
- Header `X-RunGap-HMAC-SHA256`: hex-encoded HMAC-SHA256 of the **raw request body**, keyed with the athlete's fitsel passkey.
- Responses: `202` accepted; `400` invalid multipart or no FIT part; `401` bad signature; `404` unknown webhook token; `500` server error, so retry later.
- Uploading the same FIT file twice is safe: fitsel derives activity IDs from file content.
- The athlete gets the webhook token and passkey from fitsel's setup page.

## Out of scope
- Any hosted or multi-user service, and storing any athlete's Garmin key anywhere but on their own machine.
- Garmin's Business/Developer Program (push delivery, OAuth apps). That is a separate future decision.
- Health, sleep and women's-health data; Courses and Training (sending data to the watch).
- A GUI, or any change to fitsel itself.
- Publishing the release: the team builds the binaries; I tag and publish.

## Constraints
- Budget: no running cost; it runs on the athlete's machine. No consultant budget beyond the default.
- Hosting / exposure: nothing deployed. The deliverable is binaries built by CI for macOS (arm64), Linux (amd64, arm64) and Windows (amd64), ready for me to tag as a release.
- Credentials: the company never holds, requests or uses a real Garmin key or fitsel token, mine included. No test, CI job or desk calls Garmin's or fitsel's real servers. The only sources are Garmin's public documentation and the fitsel contract in this pitch.
- Data: the Garmin key, the fitsel passkey and FIT files are personal data. The tool never logs or prints the keys, and reads them from environment variables or a config file it creates readable only by the athlete (mode 0600 on macOS and Linux).
- Licence: ask me before the first release. Language: English.
- Tech: Go, standard library first (tech defaults). No Python.

## Decided already
- A client the athlete runs, not a syncer service, because of Garmin's terms (see References).
- fitsel's upload contract above is fixed; this product adapts to it.
- Build from Garmin's public Personal API documentation only. Where it is silent (history depth, rate limits, error bodies), choose a safe default (for example: back off on `429`, honour `Retry-After`), write the assumption in the README, and keep going. Don't wait for me to confirm it against the real API.
- First, a Researcher desk fetches Garmin's public Personal API description and commits it to the repo with notes on what it leaves out. The fake Garmin server is built from that committed copy, so developer desks never need to reach Garmin's site.

## The team decides
- Command and flag names beyond `--since` and `--dry-run`, config format, state file format and location.
- How to schedule it on each OS (documented recipes are enough; no installer).
- Package layout, testing approach, CI build setup, and how the fake Garmin server follows Garmin's API description.

## Ask me
- If a Researcher desk cannot reach developer.garmin.com: say so, and I'll provide the public docs.
- The licence, and the v1 release itself.
- Anything that breaks a constraint or changes "Must" / "Out of scope".

## References
- Garmin Connect Developer Program overview: https://developer.garmin.com/gc-developer-program/overview/
- Personal API FAQ (intended use, 24h tokens, no refresh tokens, key lifetimes from 30 days to 1 year): https://developer.garmin.com/gc-developer-program/personal/personal-faq/
- Personal API (token endpoint, link to the API description): https://developer.garmin.com/gc-developer-program/personal/personal-api
- Unverified reports that FIT files from Garmin's API may lack undocumented fields (VO2 max, recovery time): https://the5krunner.com/2026/10/22/garmin-personal-api/. Not the team's to check; I'll look at it during my own test.
