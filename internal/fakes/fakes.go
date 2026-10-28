// Test doubles for the walking skeleton: a fake Garmin server built from the
// committed Personal API description, and a fake fitsel endpoint that checks the
// multipart body and the HMAC signature. Both are httptest servers, so the
// tests run with no network access.
package fakes

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// FakeGarmin is a fake Garmin Personal API server.
type FakeGarmin struct {
	Server     *httptest.Server
	Key        string // the API key it accepts
	Tokens     int    // how many tokens have been issued
	Downloads  int    // how many activity files have been downloaded
	activities []FakeActivity
	mu         sync.Mutex
}

// FakeActivity is one activity the fake Garmin serves.
type FakeActivity struct {
	ID           int64  `json:"activityId"`
	Name         string `json:"activityName"`
	StartTimeGMT string `json:"startTimeGMT"`
}

// NewActivities returns the sample activities used across the tests.
func NewActivities() []FakeActivity {
	return []FakeActivity{
		{ID: 1001, Name: "Morning Run", StartTimeGMT: "2026-10-20T07:00:00Z"},
		{ID: 1002, Name: "Evening Ride", StartTimeGMT: "2026-10-25T18:30:00Z"},
		{ID: 1003, Name: "Weekend Hike", StartTimeGMT: "2026-10-27T09:15:00Z"},
	}
}

// NewFakeGarmin returns a fake that accepts key and serves these activities.
func NewFakeGarmin(key string, activities []FakeActivity) *FakeGarmin {
	f := &FakeGarmin{Key: key, activities: activities}
	mux := http.NewServeMux()
	mux.HandleFunc("/personalapitoken-service/personal-api/personal-api-key/token", f.handleToken)
	mux.HandleFunc("/activitylist-service/activities/search/activities", f.handleList)
	mux.HandleFunc("/download-service/files/activity/", f.handleDownload)
	f.Server = httptest.NewServer(mux)
	return f
}

// URL is the base URL to point garmin.Client at.
func (f *FakeGarmin) URL() string { return f.Server.URL }

// Close stops the fake server.
func (f *FakeGarmin) Close() { f.Server.Close() }

func (f *FakeGarmin) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "ApiKey "+f.Key {
		http.Error(w, "bad key", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	f.Tokens++
	f.mu.Unlock()
	// The real Personal API token endpoint returns the token as a plain-text
	// body (text/plain), not JSON: serve it the same way so the client's
	// plain-text reader is what the tests exercise.
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, "fake-token-"+fmt.Sprint(f.Tokens))
}

func (f *FakeGarmin) handleList(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer fake-token-") {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	startDate := r.URL.Query().Get("startDate")
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []FakeActivity
	for _, a := range f.activities {
		if startDate == "" || a.StartTimeGMT >= startDate {
			out = append(out, a)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (f *FakeGarmin) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/download-service/files/activity/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/download-service/files/activity/")
	var id int64
	fmt.Sscanf(idStr, "%d", &id)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.activities {
		if a.ID == id {
			// Return a ZIP containing a single .fit, as the real API does.
			zw := bufio.NewWriter(w)
			z := zip.NewWriter(zw)
			if ff, err := z.Create(a.Name + ".fit"); err == nil {
				ff.Write([]byte("FIT-DATA-" + fmt.Sprint(id)))
			}
			z.Close()
			zw.Flush()
			w.Header().Set("Content-Type", "application/x-zip-compressed")
			f.Downloads++
			return
		}
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// FakeFitsel is a fake fitsel upload endpoint that verifies the contract.
type FakeFitsel struct {
	Server    *httptest.Server
	Passkey   string
	Uploadees [][]byte
	Counts    map[string]int
	// FailOnce makes the next single upload return 500, so a caller sees an
	// upload error and stops — a light way to simulate a run killed mid-way
	// without a real signal. Cleared after it fires once.
	FailOnce bool
	mu       sync.Mutex
}

// NewFakeFitsel returns a fake that checks the multipart body and HMAC.
func NewFakeFitsel(passkey string) *FakeFitsel {
	f := &FakeFitsel{Passkey: passkey, Counts: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/ingest/wh/", f.handleIngest)
	f.Server = httptest.NewServer(mux)
	return f
}

// URL is the base URL to point fitsel.Client at.
func (f *FakeFitsel) URL() string { return f.Server.URL }

// Close stops the fake server.
func (f *FakeFitsel) Close() { f.Server.Close() }

// Count returns how many uploads of the given FIT content have been received.
func (f *FakeFitsel) Count(fit []byte) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Counts[string(fit)]
}

func (f *FakeFitsel) handleIngest(w http.ResponseWriter, r *http.Request) {
	// Fail once, if asked: the caller sees an upload error and stops, which
	// resumes on the next run. Cleared under the same lock that guards Counts
	// so the first run's accepted uploads and its failed upload are ordered.
	f.mu.Lock()
	failed := f.FailOnce
	f.FailOnce = false
	f.mu.Unlock()
	if failed {
		http.Error(w, "simulated failure", http.StatusInternalServerError)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	// Verify the HMAC of the raw body.
	want := "X-RunGap-HMAC-SHA256"
	if r.Header.Get(want) != hmacSHA256Hex(body, f.Passkey) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	// Verify the multipart body has a "fit" part.
	ctype := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ctype, "multipart/form-data") {
		http.Error(w, "not multipart", http.StatusBadRequest)
		return
	}
	_, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		http.Error(w, "bad content type", http.StatusBadRequest)
		return
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var hasFit bool
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "bad part", http.StatusBadRequest)
			return
		}
		if part.FormName() == "fit" {
			hasFit = true
			data, _ := io.ReadAll(part)
			f.mu.Lock()
			f.Uploadees = append(f.Uploadees, data)
			f.Counts[string(data)]++
			f.mu.Unlock()
		}
		part.Close()
	}
	if !hasFit {
		http.Error(w, "no FIT part", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func hmacSHA256Hex(body []byte, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
