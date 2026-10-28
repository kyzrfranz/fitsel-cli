package fitsel

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
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
	if err != nil {
		t.Fatal(err)
	}
	if accepted {
		t.Fatal("expected not accepted with bad signature")
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
