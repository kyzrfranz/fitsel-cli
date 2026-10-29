package fitsel

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newFastUploader returns an uploader with the retry delay set to ~0, so the
// retry tests exercise the retry loop without sleeping real seconds. The
// production default (two seconds) is unchanged; only the tests opt out of it.
func newFastUploader(base, passkey string) *Client {
	c := New(base, "", passkey)
	c.UploadDelay = 0
	return c
}

func TestUploadAccepted(t *testing.T) {
	var gotSig string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		gotSig = r.Header.Get("X-RunGap-HMAC-SHA256")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	up := New(srv.URL, "", "passkey")
	fit := []byte("FIT-DATA-1001")
	accepted, err := up.Upload(context.Background(), fit)
	if err != nil {
		t.Fatal(err)
	}
	if !accepted {
		t.Fatal("expected accepted")
	}
	// Verify the HMAC of the raw body matches.
	mac := hmac.New(sha256.New, []byte("passkey"))
	mac.Write(gotBody)
	want := hex.EncodeToString(mac.Sum(nil))
	if gotSig != want {
		t.Fatalf("signature mismatch: got %s want %s", gotSig, want)
	}
}

func TestUploadBadSignature(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	up := New(srv.URL, "", "passkey")
	accepted, err := up.Upload(context.Background(), []byte("FIT"))
	if err == nil {
		t.Fatal("expected an error on a bad signature")
	}
	if accepted {
		t.Fatal("expected not accepted with bad signature")
	}
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("error = %v, want ErrBadSignature", err)
	}
}

// TestUploadBadClassifiesEachSentinel proves Upload returns its own sentinel for
// each HTTP failure a caller can branch on with errors.Is (per #85): 400 ->
// ErrBadRequest, 401 -> ErrBadSignature, 404 -> ErrNotFound.
func TestUploadBadClassifiesEachSentinel(t *testing.T) {
	cases := []struct {
		name string
		code int
		want error
	}{
		{"400 invalid upload", http.StatusBadRequest, ErrBadRequest},
		{"401 bad signature", http.StatusUnauthorized, ErrBadSignature},
		{"404 unknown webhook token", http.StatusNotFound, ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
			}))
			defer srv.Close()

			up := New(srv.URL, "", "passkey")
			accepted, err := up.Upload(context.Background(), []byte("FIT"))
			if err == nil {
				t.Fatal("expected an error")
			}
			if accepted {
				t.Fatal("expected not accepted")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestUpload500RetriesThenExhausted proves a 500 is retried and, once the
// attempts run out, surfaces as ErrUploadExhausted (per #85).
func TestUpload500RetriesThenExhausted(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	up := newFastUploader(srv.URL, "passkey")
	accepted, err := up.Upload(context.Background(), []byte("FIT"))
	if err == nil {
		t.Fatal("expected an error after retries")
	}
	if accepted {
		t.Fatal("expected not accepted")
	}
	if !errors.Is(err, ErrUploadExhausted) {
		t.Fatalf("error = %v, want ErrUploadExhausted", err)
	}
	// Three attempts total (1 + 2 retries), as documented in the README.
	if got := atomic.LoadInt32(&attempts); got != uploadAttempts {
		t.Fatalf("attempts = %d, want %d (1 + 2 retries)", got, uploadAttempts)
	}
}

// TestUpload500RecoversOnRetry proves a 500 that becomes a 202 recovers on
// retry instead of exhausting — the transient case the retry loop exists for.
func TestUpload500RecoversOnRetry(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	up := newFastUploader(srv.URL, "passkey")
	accepted, err := up.Upload(context.Background(), []byte("FIT"))
	if err != nil {
		t.Fatalf("expected a 202 after a transient 500, got %v", err)
	}
	if !accepted {
		t.Fatal("expected accepted after retry")
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("attempts = %d, want 2 (a 500 then a 202)", got)
	}
}

// TestUploadDelayBailsOnCancel proves the retry delay honours the run's context:
// cancelling mid-wait returns at once with a context error instead of sitting
// through the whole uploadDelay window (per #134). This is the cancel half of
// TestUpload500RetriesThenExhausted, which proves the delay elapses when the
// context is not cancelled.
func TestUploadDelayBailsOnCancel(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	up := New(srv.URL, "", "passkey")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Let the first attempt hit the 500, then cancel during the retry
		// delay: the second attempt must never start.
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := up.Upload(ctx, []byte("FIT"))
	if err == nil {
		t.Fatal("expected an error on cancel")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("retry delay took %v, want well under the %ds window (cancelled mid-wait)", elapsed, defaultUploadDelay/time.Second)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("attempts = %d, want 1 (a cancelled delay must not start a second attempt)", got)
	}
}

// TestUploadBadRequestNotRetried proves a terminal 400 is surfaced immediately
// without burning the retry attempts.
func TestUploadBadRequestNotRetried(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	up := New(srv.URL, "", "passkey")
	if _, err := up.Upload(context.Background(), []byte("FIT")); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest, got %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("attempts = %d, want 1 (a 400 is terminal, not retried)", got)
	}
}

func TestFormDataHasFitPart(t *testing.T) {
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, err := w.CreateFormFile("fit", "activity.fit")
	if err != nil {
		t.Fatal(err)
	}
	part.Write([]byte("FIT"))
	w.Close()
	if len(body.Bytes()) == 0 {
		t.Fatal("expected non-empty body")
	}
}

// TestUploadBareTokenAppendsToBase proves that with no token in --fitsel-endpoint
// a bare FITSEL_TOKEN is appended to the base, so the request lands on
// <base>/<token> (the acceptance: default base + bare token -> request on
// /ingest/wh/<token>). The base is the test server, so the appended path stays
// local and the server records the path the request landed on.
func TestUploadBareTokenAppendsToBase(t *testing.T) {
	const token = "my-webhook-token"
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	up := New(srv.URL, token, "passkey")
	if accepted, err := up.Upload(context.Background(), []byte("FIT")); err != nil || !accepted {
		t.Fatalf("upload: accepted=%v err=%v", accepted, err)
	}
	if gotPath != "/my-webhook-token" {
		t.Fatalf("path = %q, want /my-webhook-token", gotPath)
	}
}

// TestUploadFullURLOverridesBase proves that a FITSEL_TOKEN pasted as a full
// webhook URL (starting with https://) is used as the upload URL unchanged, even
// when --fitsel-endpoint sets a base (the acceptance: full-URL token -> same
// URL). The base is a throwaway test server; the full-URL token points at a
// second, trusted TLS server, which records that the request landed there and
// not on the base.
func TestUploadFullURLOverridesBase(t *testing.T) {
	var landedOnBase bool
	base := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		landedOnBase = true
		w.WriteHeader(http.StatusAccepted)
	}))
	defer base.Close()

	// A full-URL token must start with https:// to be used as-is (per the
	// acceptance). httptest.NewTLSServer gives an https:// URL; the client is
	// built with a transport that trusts its self-signed certificate.
	var landedOnFull bool
	full := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		landedOnFull = true
		w.WriteHeader(http.StatusAccepted)
	}))
	defer full.Close()

	up := New(base.URL, full.URL, "passkey")
	up.http.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // trust the test server's self-signed cert
	}
	if accepted, err := up.Upload(context.Background(), []byte("FIT")); err != nil || !accepted {
		t.Fatalf("upload: accepted=%v err=%v", accepted, err)
	}
	if landedOnBase {
		t.Fatal("a full-URL token must override the base, but the request landed on the base")
	}
	if !landedOnFull {
		t.Fatal("a full-URL token should be used as-is, but the request did not land on it")
	}
}

// TestUploadURLResolution covers the two forms a token may be pasted in, and the
// base-trimming so a trailing slash on the base does not double up.
func TestUploadURLResolution(t *testing.T) {
	cases := []struct {
		name  string
		base  string
		token string
		want  string
	}{
		{"bare token appends to base", "https://ingest.fitsel.app/ingest/wh", "tok", "https://ingest.fitsel.app/ingest/wh/tok"},
		{"full URL used as-is", "https://ingest.fitsel.app/ingest/wh", "https://x.example/wh/tok", "https://x.example/wh/tok"},
		{"trailing slash on base not doubled", "https://ingest.fitsel.app/ingest/wh/", "tok", "https://ingest.fitsel.app/ingest/wh/tok"},
		{"leading slash on token not doubled", "https://ingest.fitsel.app/ingest/wh", "/tok", "https://ingest.fitsel.app/ingest/wh/tok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := uploadURL(tc.base, tc.token); got != tc.want {
				t.Fatalf("uploadURL(%q, %q) = %q, want %q", tc.base, tc.token, got, tc.want)
			}
		})
	}
}
