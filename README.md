# garmin-sync

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
