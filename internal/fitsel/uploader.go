// Package fitsel uploads a FIT file to the athlete's fitsel webhook using the
// fixed upload contract given in the pitch. This contract is not to be changed.
//
// Contract:
//   - POST https://ingest.fitsel.app/ingest/wh/<webhook-token>
//   - Body: multipart/form-data with the FIT in a part named "fit"
//     ("file" and "activity" are also accepted). An optional "metadata" part
//     (JSON) is logged and otherwise ignored.
//   - Header X-RunGap-HMAC-SHA256: hex HMAC-SHA256 of the raw request body,
//     keyed with the athlete's fitsel passkey.
//   - Responses: 202 accepted; 400 invalid multipart/no FIT part; 401 bad
//     signature; 404 unknown webhook token; 500 server error, retry later.
package fitsel

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"mime"
	"mime/multipart"
	"net/http"
	"time"
)

// Client uploads FIT files to a fitsel webhook.
type Client struct {
	webhookURL string
	passkey    string
	http       *http.Client
}

// New returns an uploader for the athlete's webhook and passkey.
func New(webhookURL, passkey string) *Client {
	return &Client{webhookURL: webhookURL, passkey: passkey, http: &http.Client{Timeout: 60 * time.Second}}
}

// Upload sends a FIT file and reports whether fitsel accepted it (202).
func (c *Client) Upload(fit []byte) (bool, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("fit", "activity.fit")
	if err != nil {
		return false, err
	}
	if _, err := part.Write(fit); err != nil {
		return false, err
	}
	if err := writer.Close(); err != nil {
		return false, err
	}

	sig := hmacSHA256Hex(body.Bytes(), c.passkey)
	req, err := http.NewRequest(http.MethodPost, c.webhookURL, bytes.NewReader(body.Bytes()))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-RunGap-HMAC-SHA256", sig)
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusAccepted, nil
}

// hmacSHA256Hex returns the hex HMAC-SHA256 of the raw body keyed by passkey.
func hmacSHA256Hex(body []byte, passkey string) string {
	mac := hmac.New(sha256.New, []byte(passkey))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// ContentType returns the multipart content type, used by tests.
func ContentType() string {
	return mime.FormatMediaType("multipart/form-data", map[string]string{
		"boundary": "test-boundary",
	})
}
