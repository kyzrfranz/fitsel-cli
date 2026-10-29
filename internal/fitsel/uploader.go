// Package fitsel uploads a FIT file to the athlete's fitsel webhook using the
// fixed upload contract given in the pitch. This contract is not to be changed.
//
// Contract:
//   - POST <base>/<webhook-token>, base defaulting to
//     https://ingest.fitsel.app/ingest/wh
//   - Body: multipart/form-data with the FIT in a part named "fit"
//     ("file" and "activity" are also accepted). An optional "metadata" part
//     (JSON) is logged and otherwise ignored.
//   - Header X-RunGap-HMAC-SHA256: hex HMAC-SHA256 of the raw request body,
//     keyed with the athlete's fitsel passkey.
//   - Responses: 202 accepted; 400 invalid multipart/no FIT part; 401 bad
//     signature; 404 unknown webhook token; 500 server error, retry later.
//
// Upload classifies every failure into one of four sentinels (ErrBadRequest,
// ErrBadSignature, ErrNotFound, ErrUploadExhausted) so a caller can give each
// its own exit code. A 500 or a network error is retried a few times before it
// becomes ErrUploadExhausted; a 400/401/404 is terminal and never retried.
package fitsel

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// Sentinel errors returned by Upload, one per real failure. Callers branch on
// them with errors.Is so main can give each its own exit code.
var (
	// ErrBadRequest is returned when fitsel rejects the upload as malformed
	// (400): the multipart body has no FIT part or is otherwise invalid.
	// Terminal — a retry would fail the same way.
	ErrBadRequest = errors.New("fitsel: invalid upload")

	// ErrBadSignature is returned when fitsel rejects the HMAC signature
	// (401): the passkey is wrong and will not work until the athlete copies
	// the right one from fitsel's setup page. Terminal — like a bad Garmin
	// key, it will not fix itself.
	ErrBadSignature = errors.New("fitsel: bad signature")

	// ErrNotFound is returned when fitsel does not know the webhook token
	// (404): the webhook URL is wrong. Terminal — a retry would fail the
	// same way.
	ErrNotFound = errors.New("fitsel: webhook token not found")

	// ErrUploadExhausted is returned when fitsel returns a 500 or the upload
	// cannot reach fitsel at all, after the retries are used up. The run
	// stops cleanly and nothing is marked synced.
	ErrUploadExhausted = errors.New("fitsel: upload failed after retries")
)

// uploadAttempts is the total number of upload attempts (1 initial + retries).
// The pitch says "retry a few times" without a count; three is a safe default
// that gives a transient 500 or a momentary network drop a chance to recover
// without making a broken webhook wait long (see the README).
const uploadAttempts = 3

// defaultUploadDelay is the fixed wait between upload attempts. A couple of
// seconds lets a transient server error or network blip clear before the next
// try. It is the default; a client may override it with its UploadDelay field
// (tests set it to ~0 so the retry loop does not sleep real seconds).
const defaultUploadDelay = 2 * time.Second

// UploadDelayDelay is the production wait between upload attempts. It is a var,
// not a const, so tests can point it at ~0 (see the README); production leaves
// it at its default of two seconds.
var UploadDelayDelay = defaultUploadDelay

// Client uploads FIT files to a fitsel webhook.
type Client struct {
	webhookURL string
	passkey    string
	http       *http.Client
	// UploadDelay is the wait between upload attempts on a retryable failure
	// (500 / network). It defaults to two seconds (see New); a caller may set
	// it to anything else — tests set it to ~0 so the retry loop does not sleep
	// real seconds. A zero value means "wait no time at all".
	UploadDelay time.Duration
}

// New returns an uploader for the athlete's webhook and passkey.
//
// base is the fitsel webhook base URL (the part --fitsel-endpoint overrides);
// the upload goes to base/<token>. token is the athlete's FITSEL_TOKEN, which
// may be pasted either as a bare webhook token or as a full webhook URL:
//
//   - if token starts with "https://", it is used as the full upload URL as-is;
//   - otherwise it is appended to base, giving base + "/" + token.
//
// So a bare token with the default base uploads to
// https://ingest.fitsel.app/ingest/wh/<token>, and a full-URL token uploads to
// exactly that URL.
func New(base, token, passkey string) *Client {
	return &Client{webhookURL: uploadURL(base, token), passkey: passkey, http: &http.Client{Timeout: 60 * time.Second}, UploadDelay: defaultUploadDelay}
}

// uploadURL resolves the full upload URL from the base and the token, honouring
// the two forms a token may be pasted in (a bare token, or a full URL).
func uploadURL(base, token string) string {
	if strings.HasPrefix(token, "https://") {
		return token
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(token, "/")
}

// Upload sends a FIT file and reports whether fitsel accepted it (202). It
// returns nil only on 202; every other outcome returns a sentinel error that a
// caller can branch on with errors.Is:
//
//   - 400 invalid multipart/no FIT part  -> ErrBadRequest
//   - 401 bad signature                   -> ErrBadSignature
//   - 404 unknown webhook token           -> ErrNotFound
//   - 500 / network failure (all attempts) -> ErrUploadExhausted
//
// A 500 or network failure is retried (see uploadAttempts); a 400/401/404 is
// terminal and never retried.
//
// The retry delay honours ctx: if the caller's context is cancelled (a Ctrl-C)
// before the delay elapses, Upload returns ctx.Err() instead of waiting, so a
// stop reaches the upload loop rather than hanging on the delay.
func (c *Client) Upload(ctx context.Context, fit []byte) (bool, error) {
	var lastErr error
	for attempt := 1; attempt <= uploadAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		accepted, err := c.uploadOnce(ctx, fit)
		if err == nil {
			return accepted, nil
		}
		// A malformed request or a bad key/token will not fix itself on a
		// retry; surface it immediately rather than burning the attempts.
		if errors.Is(err, ErrBadRequest) || errors.Is(err, ErrBadSignature) || errors.Is(err, ErrNotFound) {
			return false, err
		}
		lastErr = err
		if attempt < uploadAttempts {
			// Wait for the retry delay, but bail early if the caller stops.
			// An UploadDelay of zero means "no wait", which the tests set so
			// the retry loop does not sleep real seconds; otherwise the
			// production delay (two seconds by default) applies.
			timer := time.NewTimer(c.UploadDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return false, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return false, fmt.Errorf("%w: %v", ErrUploadExhausted, lastErr)
}

// uploadOnce builds the multipart body, signs it, and sends a single request,
// classifying the response into a sentinel. It returns nil only on 202.
func (c *Client) uploadOnce(ctx context.Context, fit []byte) (bool, error) {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.webhookURL, bytes.NewReader(body.Bytes()))
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

	switch {
	case resp.StatusCode == http.StatusAccepted:
		return true, nil
	case resp.StatusCode == http.StatusBadRequest:
		return false, ErrBadRequest
	case resp.StatusCode == http.StatusUnauthorized:
		return false, ErrBadSignature
	case resp.StatusCode == http.StatusNotFound:
		return false, ErrNotFound
	default:
		return false, fmt.Errorf("fitsel: request returned %s", resp.Status)
	}
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
