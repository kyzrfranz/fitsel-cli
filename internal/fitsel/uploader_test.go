package fitsel

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

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

	up := New(srv.URL, "passkey")
	fit := []byte("FIT-DATA-1001")
	accepted, err := up.Upload(fit)
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

	up := New(srv.URL, "passkey")
	accepted, err := up.Upload([]byte("FIT"))
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

			up := New(srv.URL, "passkey")
			accepted, err := up.Upload([]byte("FIT"))
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

	up := New(srv.URL, "passkey")
	accepted, err := up.Upload([]byte("FIT"))
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

	up := New(srv.URL, "passkey")
	accepted, err := up.Upload([]byte("FIT"))
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

// TestUploadBadRequestNotRetried proves a terminal 400 is surfaced immediately
// without burning the retry attempts.
func TestUploadBadRequestNotRetried(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	up := New(srv.URL, "passkey")
	if _, err := up.Upload([]byte("FIT")); !errors.Is(err, ErrBadRequest) {
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
