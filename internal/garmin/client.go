// Package garmin is a client for Garmin's public Personal API: it exchanges the
// athlete's Personal API key for a 24h access token, lists activities since a
// start date, and downloads each as an original FIT file.
//
// It is built from the committed copy of Garmin's public Personal API
// description (docs/garmin/). Where that description is silent, it uses a safe
// default, documented in the README:
//   - the token response is read from the "access_token" field (the FAQ only
//     says "an access token");
//   - the token is sent as "Authorization: Bearer *** (the FAQ says the APIs
//     use OAuth2 access tokens);
//   - a downloaded file comes back as a ZIP (application/x-zip-compressed), so
//     the single .fit inside is taken.
package garmin

import (
	"context"
	"encoding/json"
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

// Client talks to Garmin's Personal API.
type Client struct {
	baseURL string
	key     string
	http    *http.Client
}

// New returns a client for the athlete's key.
func New(key string) *Client {
	return &Client{baseURL: BaseURL, key: key, http: &http.Client{Timeout: 30 * time.Second}}
}

// SetBaseURL overrides the host; used by tests to point at the fake server.
func (c *Client) SetBaseURL(u string) { c.baseURL = strings.TrimRight(u, "/") }

// tokenResponse is the shape of the token endpoint's body. The OpenAPI file
// does not document it; "access_token" is the documented OAuth2 field name.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// AccessToken is a Garmin access token.
type AccessToken struct {
	token   string
	expires time.Time
}

// Valid reports whether the token is still fresh (a minute's grace).
func (t AccessToken) Valid() bool { return time.Now().Before(t.expires.Add(-time.Minute)) }

// Authorize exchanges the key for an access token.
func (c *Client) Authorize(ctx context.Context) (AccessToken, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/personalalapitoken-service/personal-api/personal-api-key/token", nil)
	if err != nil {
		return AccessToken{}, err
	}
	req.Header.Set("Authorization", "ApiKey ***"+c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return AccessToken{}, fmt.Errorf("requesting token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return AccessToken{}, fmt.Errorf("garmin: token endpoint returned %s", resp.Status)
	}
	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return AccessToken{}, fmt.Errorf("decoding token response: %w", err)
	}
	if tr.AccessToken == "" {
		return AccessToken{}, fmt.Errorf("garmin: token response had no access_token")
	}
	expires := time.Now().Add(24 * time.Hour)
	if tr.ExpiresIn > 0 {
		expires = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return AccessToken{token: tr.AccessToken, expires: expires}, nil
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
func (c *Client) List(ctx context.Context, token AccessToken, startDate string) ([]Activity, error) {
	q := url.Values{}
	q.Set("start", "0")
	q.Set("limit", "100")
	q.Set("startDate", startDate)
	q.Set("sortBy", "beginTimestamp")
	q.Set("sortOrder", "desc")
	u := fmt.Sprintf("%s/activitylist-service/activities/search/activities?%s", c.baseURL, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing activities: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("garmin: list returned %s", resp.Status)
	}
	var acts []activity
	if err := json.NewDecoder(resp.Body).Decode(&acts); err != nil {
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

// Download fetches an activity's file and returns the FIT bytes inside the
// ZIP archive Garmin returns.
func (c *Client) Download(ctx context.Context, token AccessToken, id int64) ([]byte, error) {
	u := fmt.Sprintf("%s/download-service/files/activity/%d", c.baseURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading activity %d: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("garmin: download of %d returned %s", id, resp.Status)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return unzipFit(raw)
}
