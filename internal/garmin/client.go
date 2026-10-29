// Package garmin is a client for Garmin's public Personal API: it exchanges the
// athlete's Personal API key for a 24h access token, lists activities since a
// start date, and downloads each as an original FIT file.
//
// It is built from the committed copy of Garmin's public Personal API
// description (docs/garmin/). Where that description is silent, it uses a safe
// default, documented in the README:
//   - the token response is the token itself as a plain-text body (the FAQ only
//     says "an access token", and the real endpoint returns text/plain, not
//     JSON);
//   - the token is sent as "Authorization: Bearer <token>" (the FAQ says the
//     APIs use OAuth2 access tokens);
//   - a downloaded file comes back as a ZIP (application/x-zip-compressed), so
//     the single .fit inside is taken.
package garmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// BaseURL is the real Garmin Personal API host. Tests point the client at the
// fake server instead.
const BaseURL = "https://personal.connectapi.garmin.com"

// ErrBadKey is returned when the Garmin key is rejected (401) at the token
// endpoint: the key is invalid and will not work until the athlete creates a
// new one. Callers branch on it with errors.Is.
var ErrBadKey = errors.New("garmin: invalid API key")

// ErrRateLimited is returned when Garmin rate-limits a protected call (429): the
// client backs off for Retry-After (a safe default if the header is absent) and
// returns this once, so the run stops cleanly instead of hammering the API.
// Callers branch on it with errors.Is.
var ErrRateLimited = errors.New("garmin: rate limited")

// defaultRetryAfter is the back-off the client honours when Garmin returns a
// 429 without a Retry-After header: the Personal API does not document it, so
// this is a safe default rather than an immediate retry (see the README).
const defaultRetryAfter = 30 * time.Second

// RetryAfterDelay is the production back-off when Garmin rate-limits a call
// without a Retry-After header. It is a var, not a const, so tests can point it
// at ~0 (see the README); production leaves it at its default of 30 seconds.
var RetryAfterDelay = defaultRetryAfter

// retryAfter parses a Retry-After header (Garmin's Personal API does not
// document it) and returns how long to wait before giving up. It accepts the
// two forms the HTTP spec allows — an absolute HTTP date or a number of
// seconds — and falls back to a safe default when the header is missing or
// unparseable, so a malformed header never triggers an immediate retry.
func retryAfter(c *Client, h string) time.Duration {
	if h == "" {
		// No Retry-After header: fall back to the client's configured back-off.
		// A client may override defaultRetryAfter with its RetryAfter field
		// (tests set it to ~0 so the back-off does not sleep real seconds); a
		// zero value means "wait no time at all". Otherwise the production
		// back-off (retryAfterDelay, 30 seconds by default) applies.
		if c.RetryAfter > 0 {
			return c.RetryAfter
		}
		return RetryAfterDelay
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return defaultRetryAfter
}

// Client talks to Garmin's Personal API. It owns its access token: once
// Authorize has succeeded, List and Download refresh it transparently when it
// expires or Garmin rejects it (a 401), so a token expiring mid-run does not
// stop the run — the client just asks for a new one.
type Client struct {
	baseURL string
	key     string
	http    *http.Client

	// RetryAfter is the back-off the client honours when Garmin rate-limits a
	// call (a 429) without a Retry-After header. It defaults to 30 seconds
	// (see New); a caller may override it — tests set it to ~0 so the back-off
	// does not sleep real seconds. A zero value means "wait no time at all".
	RetryAfter time.Duration

	token AccessToken // set by Authorize; refreshed by bearer()
}

// New returns a client for the athlete's key.
func New(key string) *Client {
	return &Client{baseURL: BaseURL, key: key, http: &http.Client{Timeout: 30 * time.Second}, RetryAfter: defaultRetryAfter}
}

// SetBaseURL overrides the host; used by tests to point at the fake server.
func (c *Client) SetBaseURL(u string) { c.baseURL = strings.TrimRight(u, "/") }

// AccessToken is a Garmin access token.
type AccessToken struct {
	token   string
	expires time.Time
}

// Valid reports whether the token is still fresh (a minute's grace).
func (t AccessToken) Valid() bool { return time.Now().Before(t.expires.Add(-time.Minute)) }

// Authorize exchanges the key for an access token and stores it in the client,
// so subsequent calls refresh it themselves instead of taking it as an arg.
// Garmin's Personal API token endpoint returns the token as a plain-text body
// (text/plain), not JSON: the ~1.1KB response is the token itself, used as the
// Bearer for 24h.
func (c *Client) Authorize(ctx context.Context) (AccessToken, error) {
	tok, err := c.refresh(ctx)
	if err != nil {
		return AccessToken{}, err
	}
	c.token = tok
	return tok, nil
}

// bearer returns a valid access token, refreshing it if it has expired or
// Garmin has rejected the last one (a 401). A refresh that itself 401s means
// the key is bad, not the token, so it surfaces as ErrBadKey.
func (c *Client) bearer(ctx context.Context) (AccessToken, error) {
	if c.token.Valid() {
		return c.token, nil
	}
	tok, err := c.refresh(ctx)
	if err != nil {
		return AccessToken{}, err
	}
	c.token = tok
	return tok, nil
}

// refresh hits the token endpoint and stores the new token in the client.
func (c *Client) refresh(ctx context.Context) (AccessToken, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/personalapitoken-service/personal-api/personal-api-key/token", nil)
	if err != nil {
		return AccessToken{}, err
	}
	req.Header.Set("Authorization", "ApiKey "+c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return AccessToken{}, fmt.Errorf("requesting token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			return AccessToken{}, ErrBadKey
		}
		return AccessToken{}, fmt.Errorf("garmin: token endpoint returned %s", resp.Status)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return AccessToken{}, fmt.Errorf("reading token response: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return AccessToken{}, fmt.Errorf("garmin: token response was empty")
	}
	return AccessToken{token: token, expires: time.Now().Add(24 * time.Hour)}, nil
}

// activity is the minimal shape of a SummarizedActivityDTO we need.
type activity struct {
	ActivityID   int64  `json:"activityId"`
	ActivityName string `json:"activityName"`
	StartTimeGMT string `json:"startTimeGMT"`
}

// Activity is a downloadable activity.
type Activity struct {
	ID       int64
	Name     string
	StartGMT string
}

// pageSize is the page size the list endpoint returns per request. Garmin's
// Personal API caps the activities list at 100 per request (the historical
// default), so anything past the first page must be requested by offset: the
// only way to reach an athlete's activities past the first 100 since --since.
const pageSize = 100

// List returns activities at or after startDate (YYYY-MM-DD), newest first. If
// endDate is non-empty it also returns only those on or before it, so a call
// lists the window [--since, --to]. It walks every page the endpoint returns,
// so an athlete with more than pageSize activities in the window is not silently
// truncated at the first 100: each page is requested at the running offset and
// appended, until a page returns fewer than pageSize activities (the last one).
func (c *Client) List(ctx context.Context, startDate, endDate string) ([]Activity, error) {
	out := make([]Activity, 0)
	for start := 0; ; start += pageSize {
		page, err := c.listPage(ctx, startDate, endDate, start)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		// Stop after a short page (the last) or an empty one: a full page of
		// pageSize may be the last, so do not assume there is another — the
		// next request returning nothing ends the loop.
		if len(page) < pageSize {
			break
		}
	}
	return out, nil
}

// listPage fetches one page of activities starting at offset start, newest
// first, returning just that page's activities. endDate limits the upper bound
// (empty means open-ended at the top): it is forwarded to the endpoint as its
// endDate query parameter only when non-empty, so a call without --to is
// unchanged from the open-ended --since behaviour.
func (c *Client) listPage(ctx context.Context, startDate, endDate string, start int) ([]Activity, error) {
	q := url.Values{}
	q.Set("start", strconv.Itoa(start))
	q.Set("limit", strconv.Itoa(pageSize))
	q.Set("startDate", startDate)
	if endDate != "" {
		q.Set("endDate", endDate)
	}
	q.Set("sortBy", "beginTimestamp")
	q.Set("sortOrder", "desc")
	u := fmt.Sprintf("%s/activitylist-service/activities/search/activities?%s", c.baseURL, q.Encode())
	body, err := c.get(ctx, "listing activities", u)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var acts []activity
	if err := json.NewDecoder(body).Decode(&acts); err != nil {
		return nil, fmt.Errorf("decoding activities: %w", err)
	}
	out := make([]Activity, 0, len(acts))
	for _, a := range acts {
		if a.ActivityID != 0 {
			out = append(out, Activity{ID: a.ActivityID, Name: a.ActivityName, StartGMT: a.StartTimeGMT})
		}
	}
	return out, nil
}

// get performs a GET and returns the response body (caller closes it),
// refreshing the token once if Garmin rejects it (a 401). A 401 on a protected
// endpoint means the token is bad, not the request, so bearer() refreshes it
// once and the request is retried; if that also 401s, bearer() returns
// ErrBadKey. op names the caller in any wrapped error, so a failed Download
// reports "downloading activity <id>" rather than the List's "listing
// activities".
func (c *Client) get(ctx context.Context, op, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	tok, err := c.bearer(ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		// Garmin is asking us to slow down: back off for Retry-After (a safe
		// default if the header is absent) and stop cleanly. No retry loop —
		// the run returns ErrRateLimited so main gives it its own exit code.
		resp.Body.Close()
		// Honour the run's context while waiting: a Ctrl-C bails out of the
		// back-off instead of sitting through the whole window, and the run
		// treats it as an interrupt rather than a rate limit.
		timer := time.NewTimer(retryAfter(c, resp.Header.Get("Retry-After")))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		return nil, ErrRateLimited
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// The token was rejected: force a refresh (do not go through bearer(),
		// which would return the still-valid-but-rejected token) and retry.
		resp.Body.Close()
		tok, err := c.refresh(ctx)
		if err != nil {
			// A refresh that 401s means the key is bad, not the token.
			return nil, err
		}
		c.token = tok
		req.Header.Set("Authorization", "Bearer "+tok.token)
		resp, err = c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		// The refreshed token was rejected too: the key itself is bad, not
		// just the token, so surface it as ErrBadKey rather than a raw status.
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			return nil, ErrBadKey
		}
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("garmin: request returned %s", resp.Status)
	}
	return resp.Body, nil
}

// Download fetches an activity's file and returns the FIT bytes inside the
// ZIP archive Garmin returns.
func (c *Client) Download(ctx context.Context, id int64) ([]byte, error) {
	u := fmt.Sprintf("%s/download-service/files/activity/%d", c.baseURL, id)
	body, err := c.get(ctx, fmt.Sprintf("downloading activity %d", id), u)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	return unzipFit(raw)
}
