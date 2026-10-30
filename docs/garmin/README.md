# Garmin's public Personal API description

The source for the fake Garmin server. Fetched on 2026-10-28 from Garmin's public developer pages; nothing here needs a Garmin account.

- `personal-api.openapi.json`: the OpenAPI 3.0.1 description "Garmin Connect Personal API" 1.0.0, 26 paths, as embedded in the Swagger page at https://developer.garmin.com/gc-developer-program/personal/personal-api (the page's JavaScript bundle holds it as `JSON.parse('…')`).
- `personal-api.md`, `personal-faq.md`: the text of the two pages.

## What fitsel-client uses
- Token: `POST https://personal.connectapi.garmin.com/personalapitoken-service/personal-api/personal-api-key/token` with the header `Authorization: ApiKey <key>`. It is not in the OpenAPI file; it is on both pages. Tokens last 24 hours or until the key expires; there are no refresh tokens.
- List: `GET /activitylist-service/activities/search/activities` with `start`, `limit`, `startDate`, `endDate`, `sortBy`, `sortOrder` and more. It returns an array of `SummarizedActivityDTO`, and an empty array when nothing matches. Documented responses: `200`, `400`, `401`, `403`, `500`.
- Download: `GET /download-service/files/activity/{activityId}`, "an activity file in its original format". The response is `application/x-zip-compressed`: **a ZIP archive, not a bare FIT file**. Unzip it before uploading to fitsel.

## What the description leaves out (choose a safe default and write it in the README)
- The token response body: field names, token type, expiry. The FAQ only says "an access token".
- How the access token is sent on API calls. The FAQ says "OAuth2 access tokens", so `Authorization: Bearer <token>` is the likely choice.
- Rate limits: no `429` anywhere, no `Retry-After`.
- Error bodies: only status codes are listed. Nothing tells a bad key from an expired one.
- History depth: how far back the search reaches.
- The download's error responses: only `200` is listed. Also whether the ZIP can hold more than one file.
