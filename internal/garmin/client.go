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

// Client talks to Garmin's Personal API. It owns its access token: once
// Authorize has succeeded, List and Download refresh it transparently when it
// expires or Garmin rejects it (a 401), so a token expiring mid-run does not
// stop the run — the client just asks for a new one.
type Client struct {
	baseURL string
	key     string
	http    *http.Client

	token AccessToken // set by Authorize; refreshed by bearer()
}

// New returns a client for the athlete's key.
func New(key string) *Client {
	return &Client{baseURL: BaseURL, key: key, http: &http.Client{Timeout: 30 * time.Second}}
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

// List returns activities at or after startDate (YYYY-MM-DD), newest first.
func (c *Client) List(ctx context.Context, startDate string) ([]Activity, error) {
	q := url.Values{}
	q.Set("start", "0")
	q.Set("limit", "100")
	q.Set("startDate", startDate)
	q.Set("sortBy", "beginTimestamp")
	q.Set("sortOrder", "desc")
	u := fmt.Sprintf("%s/activitylist-service/activities/search/activities?%s", c.baseURL, q.Encode())
	body, err := c.get(ctx, u)
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
// ErrBadKey.
func (c *Client) get(ctx context.Context, url string) (io.ReadCloser, error) {
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
		return nil, fmt.Errorf("listing activities: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// The token was rejected: force a refresh (do not go through bearer(),
		// which would return the still-valid-but-rejected token) and retry.
		resp.Body.Close()
		tok, err := c.refresh(ctx)
		if err != nil {
			return nil, err
		}
		c.token = tok
		req.Header.Set("Authorization", "Bearer "+tok.token)
		resp, err = c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("listing activities: %w", err)
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
	body, err := c.get(ctx, u)
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
