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
	"strconv"
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

	// PagesSeen records the start offset of each list request the server has
	// served, in order — a test reads it to prove the client paginated
	// (e.g. [0 100 200]). Empty until the first list request is served.
	PagesSeen []int

	// UnauthorizedOnce makes the next protected call (List or Download) return
	// 401 once, then recover — a light way to exercise the client's refresh-on-
	// 401 without a real expired token. Cleared after it fires once.
	UnauthorizedOnce bool

	// ExpiredOnce makes the first token issued already expired, so the next
	// protected call must refresh it — exercises the refresh-on-expiry path
	// end to end. Cleared after it fires once.
	ExpiredOnce bool

	// RefreshFails makes the token exchange fail (401) once, on the refresh
	// triggered by a rejected/expired token — exercises the "re-auth still 401s
	// is a bad key" path. Cleared after it fires once.
	RefreshFails bool

	// RateLimitOnce makes the next protected call (List or Download) return a
	// 429 once, with a Retry-After header, so a client can exercise its
	// rate-limit back-off path. Cleared after it fires once.
	RateLimitOnce bool
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
		{ID: 1001, Name: "Morning Run", StartTimeGMT: "2026-10-20 07:00:00"},
		{ID: 1002, Name: "Evening Ride", StartTimeGMT: "2026-10-25 18:30:00"},
		{ID: 1003, Name: "Weekend Hike", StartTimeGMT: "2026-10-27 09:15:00"},
	}
}

// ManyActivities returns n activities (id 1..n, newest first) all at or after
// the given start date, so a test can serve more than one page (pageSize) of
// the list endpoint — the shape a real athlete past the first 100 has. The
// list endpoint sorts by beginTimestamp desc, so they must be built newest
// first: id 1 is the newest, id n the oldest.
//
// The dates are built as ISO-8601 (T...Z) so the fake's lexicographic
// startDate filter orders them correctly; each is a full day apart so all n are
// distinct and all fall after a --since of 2026-10-01.
func ManyActivities(n int, startDate string) []FakeActivity {
	acts := make([]FakeActivity, 0, n)
	for i := 1; i <= n; i++ {
		// Day 300-i keeps them all after 2026-10-01 and ordered newest-first
		// as i grows (id 1 is the newest). A full day apart keeps each date
		// distinct and the string comparison in the fake's list handler
		// ordering correct.
		day := 300 - i
		acts = append(acts, FakeActivity{
			ID:           int64(i),
			Name:         fmt.Sprintf("Activity %d", i),
			StartTimeGMT: fmt.Sprintf("2026-12-%02dT09:00:00Z", day),
		})
	}
	return acts
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
	expired := f.ExpiredOnce
	f.ExpiredOnce = false
	f.mu.Unlock()
	// RefreshFails: the token exchange fails (401) once, on the refresh
	// triggered by a rejected/expired token (i.e. the second token request),
	// which the client surfaces as ErrBadKey (the key is bad, not the token).
	f.mu.Lock()
	if f.RefreshFails && f.Tokens > 1 {
		f.RefreshFails = false
		f.mu.Unlock()
		http.Error(w, "bad key", http.StatusUnauthorized)
		return
	}
	f.mu.Unlock()
	// The real Personal API token endpoint returns the token as a plain-text
	// body (text/plain), not JSON: serve it the same way so the client's
	// plain-text reader is what the tests exercise.
	w.Header().Set("Content-Type", "text/plain")
	if expired {
		// An already-expired token: the client must refresh it on the next
		// protected call, which is what this exercises end to end.
		fmt.Fprint(w, "expired-token-1")
	} else {
		fmt.Fprint(w, "fake-token-"+fmt.Sprint(f.Tokens))
	}
}

func (f *FakeGarmin) handleList(w http.ResponseWriter, r *http.Request) {
	// RateLimitOnce: fail the next protected call once with a 429 and a
	// Retry-After header, so a client can exercise its rate-limit back-off.
	// UnauthorizedOnce and RateLimitOnce share one lock so the first of them
	// to fire wins and the second can still run.
	f.mu.Lock()
	if f.RateLimitOnce {
		f.RateLimitOnce = false
		f.mu.Unlock()
		w.Header().Set("Retry-After", "2")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	if f.UnauthorizedOnce {
		f.UnauthorizedOnce = false
		f.mu.Unlock()
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	f.mu.Unlock()
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer fake-token-") {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	startDate := r.URL.Query().Get("startDate")
	endDate := r.URL.Query().Get("endDate")
	start, _ := strconv.Atoi(r.URL.Query().Get("start"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	f.mu.Lock()
	f.PagesSeen = append(f.PagesSeen, start)
	f.mu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	// Filter by startDate (lower bound) and endDate (upper bound), then return
	// the page [start, start+limit): this is what makes the fake page like the
	// real endpoint, so a client that ignores the offset would see the same
	// first page twice and a client that does not page would miss everything
	// past the first page. Both bounds are YYYY-MM-DD dates; the activity's start
	// carries a time, so compare the date portion (first 10 runes) of the start
	// against the bounds. A YYYY-MM-DD prefix compares chronologically, so the
	// endDate bound is inclusive of the whole end day.
	var matched []FakeActivity
	for _, a := range f.activities {
		start := a.StartTimeGMT[:10]
		startOn := startDate == "" || start >= startDate
		endOn := endDate == "" || start <= endDate
		if startOn && endOn {
			matched = append(matched, a)
		}
	}
	var out []FakeActivity
	if start < len(matched) {
		high := start + limit
		if high > len(matched) || limit == 0 {
			high = len(matched)
		}
		out = matched[start:high]
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (f *FakeGarmin) handleDownload(w http.ResponseWriter, r *http.Request) {
	// RateLimitOnce: fail the next protected call once with a 429 and a
	// Retry-After header, so a client can exercise its rate-limit back-off.
	// UnauthorizedOnce and RateLimitOnce share one lock so the first of them
	// to fire wins and the second can still run.
	f.mu.Lock()
	if f.RateLimitOnce {
		f.RateLimitOnce = false
		f.mu.Unlock()
		w.Header().Set("Retry-After", "2")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	// UnauthorizedOnce: fail the next protected call once, then recover.
	if f.UnauthorizedOnce {
		f.UnauthorizedOnce = false
		f.mu.Unlock()
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	f.mu.Unlock()
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

	// Statuses is a queue of HTTP statuses consumed one per upload, so a test
	// can script a sequence — e.g. 500, 500, 202 for the retry path, or a
	// single 404. When empty (the default) every upload returns 202. Consumed
	// under mu alongside FailOnce and Counts.
	Statuses []int
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
	// Decide this upload's fate up front, under one lock so it is ordered
	// against FailOnce and Counts. FailOnce still wins (a run killed mid-way):
	// the existing resume test relies on it.
	f.mu.Lock()
	switch {
	case f.FailOnce:
		f.FailOnce = false
		f.mu.Unlock()
		http.Error(w, "simulated failure", http.StatusInternalServerError)
		return
	case len(f.Statuses) > 0:
		status := f.Statuses[0]
		f.Statuses = f.Statuses[1:]
		f.mu.Unlock()
		http.Error(w, "scripted failure", status)
		return
	default:
		f.mu.Unlock()
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
