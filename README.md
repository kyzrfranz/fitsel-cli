# garmin-sync

## Exit codes

A run stops with one of these codes so an athlete (or a cron) can act on it:

| Code | Meaning | Do |
|---|---|---|
| 0 | Success. Synced everything that was pending. | Nothing. |
| 1 | A general failure (network, fitsel, state, first run without `--since`). | See the message on the `error:` line. |
| 3 | Your Garmin API key is invalid (Garmin returned 401). | Check `GARMIN_API_KEY` or create a new one at developer.garmin.com. |
| 5 | Garmin is rate-limiting you (429); the tool waited for the window in the `error:` line, then stopped. | Wait that long, then run again — do not retry sooner. |

More codes are added as features land (uploads). This list grows with them.
