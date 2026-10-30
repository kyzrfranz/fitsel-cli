// Package checker implements the daily background update check: it compares the
// CLI's built-in version against the latest GitHub release and, when a newer
// version exists, returns a one-line upgrade hint.
//
// It is best-effort and silent: every failure — a dial error, a >2s timeout, a
// non-2xx response, or a body that is not the expected JSON — is swallowed, so a
// broken or slow GitHub can never stop an athlete from syncing. It makes at most
// one request per day, throttled by a timestamp in the state dir. The check is a
// pure function of its inputs, so it can be tested against an httptest fake
// server with an injectable base URL and never touch the real GitHub.
package checker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Current returns the CLI's built-in version, the value main sets via
// SetVersion (defaulting to "0.0.0" so the checker's own tests have a baseline).
func Current() string { return version }

// SetVersion overrides the built-in version the checker compares against. main
// calls it with the real version (set at release by GoReleaser) so the checker
// compares the shipped version; it is a function so tests can restore it.
func SetVersion(v string) { version = v }

// version is the built-in version the checker compares against. main sets it via
// SetVersion; the checker's own tests set it directly. A dev build carries a
// "-dev" suffix, which the check skips (see Run).
var version = "0.0.0"

// githubRelease is the shape of the two fields the checker reads from GitHub's
// /releases/latest response. It ignores everything else the endpoint returns,
// so a change to the response shape never breaks the compare.
type githubRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

// Hint is the one-line upgrade notice returned when a newer version exists: the
// release URL and the Homebrew command the athlete runs to get it.
type Hint struct {
	URL     string // the release page, e.g. https://github.com/kyzrfranz/fitsel-cli/releases/tag/v0.1.0
	BrewCmd string // "brew upgrade fitsel-client"
	Text    string // the single line the human-facing CLI prints, built once in newHint
}

// hintFormat is the one-line hint printed in human mode: the release URL, then
// the brew command, on a single line. The design pins the shape to "the URL then
// the brew command" so tests assert both tokens without fixing the separator.
const hintFormat = "Update available — %s — run \"brew upgrade fitsel-client\""

// timeout is the wall-clock the GET to GitHub is allowed. A slow or unreachable
// GitHub must not hang a sync, so the client gives up after 2s and the check is
// treated as a swallowed failure (no hint, no crash).
const timeout = 2 * time.Second

// oncePerDay is how long to wait between checks: a check runs at most once per
// 24h, read from the last-run timestamp in the state dir.
const oncePerDay = 24 * time.Hour

// ErrOptedOut is returned when the check is disabled with --no-update-check or
// FITSEL_NO_UPDATE_CHECK=1. Callers branch on it with errors.Is.
var ErrOptedOut = errors.New("checker: update check disabled")

// ErrDevBuild is returned for a dev build (the version string contains "-dev"):
// there is nothing to check against, so the check is skipped. Callers branch on
// it with errors.Is.
var ErrDevBuild = errors.New("checker: dev build, skipping update check")

// ErrThrottled is returned when a check ran less than oncePerDay ago: the
// timestamp in the state dir says the daily check has already happened, so it is
// skipped until the next day. Callers branch on it with errors.Is.
var ErrThrottled = errors.New("checker: checked within the last 24 hours")

// ErrDisabled is the umbrella error for "the check is disabled and nothing
// should happen": it wraps ErrOptedOut and ErrDevBuild so a caller can treat all
// the no-request skip paths as one, and it is what the once-per-day throttle
// returns too. It is the single "make zero network calls" outcome.
var ErrDisabled = errors.New("checker: update check skipped")

// BaseURL is the real GitHub API host the checker queries. Tests point it at the
// httptest fake server instead. The endpoint is /releases/latest, so the full
// URL is <BaseURL>/releases/latest.
const BaseURL = "https://api.github.com"

// Base is the injectable GitHub API base URL. It defaults to the real GitHub API
// (BaseURL) and is a var so tests point the GET at their fake server.
var Base = BaseURL

// Options configures one check. Every field has a sensible default so a caller
// (main) can pass a zero Options and still get the production behaviour.
type Options struct {
	// Base overrides the GitHub API base URL. The GET goes to <Base>
	// + "/releases/latest". Leave it empty to use the default.
	Base string

	// StateDir is where the once-per-day timestamp is read from and written to.
	// Leave it empty to use the default (~/.config/fitsel-client, the state
	// file's home-dir default).
	StateDir string

	// NoUpdateCheck opts the athlete out of the check entirely (--no-update-check
	// or FITSEL_NO_UPDATE_CHECK=1). When true the check makes no request and
	// returns ErrOptedOut.
	NoUpdateCheck bool

	// Now is the time the check runs. It is a field, not a call to time.Now, so
	// the throttle test can pin it. It defaults to time.Now() when zero.
	Now time.Time

	// Version overrides the built-in version the checker compares against. It is
	// empty so main can pass the real version string; when empty the checker
	// uses the built-in version (Current).
	Version string
}

// Run performs one update check and returns a Hint when a newer version exists,
// or an error describing why the check was skipped or failed. Every error is a
// "do nothing" outcome (checked with errors.Is); a nil error with a nil Hint
// means the check ran and the latest version is equal to or older.
func Run(opts Options) (*Hint, error) {
	// The opt-out and the dev build decide first, before anything else, so they
	// make zero network calls even on the very first run. The opt-out is an
	// explicit athlete choice; a dev build has no release to compare against.
	if opts.NoUpdateCheck {
		return nil, ErrOptedOut
	}
	v := opts.Version
	if v == "" {
		v = Current()
	}
	if strings.Contains(v, "-dev") {
		return nil, ErrDevBuild
	}

	// The once-per-day throttle is the only gate that reads the disk. A first
	// run has no timestamp yet, so it proceeds; a later run within 24h of the
	// saved one is skipped. The timestamp is written on the way out, on every
	// path that reached the network (or would have), so the daily budget holds.
	if err := throttle(opts); err != nil {
		return nil, errors.Join(ErrDisabled, err)
	}

	hint, err := check(opts)
	if err != nil {
		// Every failure from check is swallowed: a dial error, the >2s timeout,
		// a non-2xx response, or a body that is not the expected JSON. The check
		// is best-effort, so none of them is reported. The timestamp is still
		// recorded, so a failed attempt counts against the daily budget and the
		// next run within 24h is throttled rather than retrying every call.
		_ = recordRun(opts)
		return nil, errors.Join(ErrDisabled, err)
	}

	// Record that the check ran now, so the next run within 24h is throttled.
	// A failure to write the timestamp is not a failure of the check.
	_ = recordRun(opts)
	return hint, nil
}

// throttle reads the last-run timestamp and, if a run happened less than
// oncePerDay ago, returns ErrThrottled. It writes nothing: recording is done by
// Run after the network call, so a throttled run does not rewrite the timestamp.
func throttle(opts Options) error {
	t, err := lastRun(opts)
	if err != nil {
		return err
	}
	if t.IsZero() {
		return nil // first run: nothing to compare against, proceed.
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	if now.Sub(t) < oncePerDay {
		return ErrThrottled
	}
	return nil
}

// lastRun reads the last-check timestamp from <StateDir>/update-check.last,
// returning the zero time when the file is absent (the first run). A malformed
// file is treated as "no record": a corrupt timestamp must not stop the check.
func lastRun(opts Options) (time.Time, error) {
	path := updateTimestampPath(opts)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, nil // corrupt timestamp: behave as "no record".
	}
	return t, nil
}

// record stamps the current time into <StateDir>/update-check.last so the next
// run within 24h is throttled. It writes atomically (temp file + rename) so a
// crash mid-write cannot corrupt it, and creates the directory if needed.
func recordRun(opts Options) error {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	ts := now.Format(time.RFC3339)
	dir := stateDirDefault(opts)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := updateTimestampPath(opts)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(ts), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// updateTimestampPath is <StateDir>/update-check.last, resolving the state dir
// to its default when the caller leaves it empty.
func updateTimestampPath(opts Options) string {
	return filepath.Join(stateDirDefault(opts), "update-check.last")
}

// stateDirDefault resolves the state dir to the default (~/.config/fitsel-client,
// or the bare "fitsel-client" dir when home is unavailable), matching the state
// package's home-dir default so the timestamp lives beside the state file.
func stateDirDefault(opts Options) string {
	if opts.StateDir != "" {
		return opts.StateDir
	}
	if p := os.Getenv("FITSEL_STATE"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "fitsel-client"
	}
	return filepath.Join(home, ".config", "fitsel-client")
}

// check makes the one GET to <Base>/releases/latest and returns a Hint when the
// latest release is newer than the built-in version. It returns an error (which
// Run swallows) for any failure: a dial error, the >2s timeout, a non-2xx
// response, or a body that is not the expected JSON.
func check(opts Options) (*Hint, error) {
	base := Base
	if opts.Base != "" {
		base = opts.Base
	}
	url := strings.TrimRight(base, "/") + "/releases/latest"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err // dial error or the >2s deadline.
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("checker: github returned %d", resp.StatusCode)
	}

	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, io.ErrUnexpectedEOF // malformed or truncated body.
	}

	return compare(rel), nil
}

// newHint builds the one-line hint from the release URL and the brew command.
// It is the single place the hint's wording lives, so the test, CLI and README
// all quote the same line.
func newHint(url string) *Hint {
	return &Hint{
		URL:     url,
		BrewCmd: "brew upgrade fitsel-client",
		Text:    fmt.Sprintf(hintFormat, url),
	}
}

// compare strips one leading "v" from the release tag and the built-in version,
// compares them numerically (major.minor.patch), and returns a Hint when the
// release is strictly newer. Equal-or-older, or an unparseable tag, returns no
// hint: the check is silent, so a pre-release never shows a spurious upgrade.
func compare(rel githubRelease) *Hint {
	latest := stripV(rel.TagName)
	current := stripV(Current())

	latestParts := splitVer(latest)
	currentParts := splitVer(current)

	// Compare component by component, treating a missing component as 0, so
	// "1.2" and "1.2.0" are equal and a tag with fewer components still compares.
	for i := 0; i < 3; i++ {
		l, c := latestParts[i], currentParts[i]
		if l > c {
			// The release endpoint carries the canonical release page in
			// html_url; use it verbatim so the hint links to exactly where the
			// release was published, wherever that is.
			return newHint(rel.HTMLURL)
		}
		if l < c {
			return nil // strictly older: nothing to show.
		}
	}
	return nil // equal: nothing to show.
}

// stripV removes at most one leading "v" (or "V"), so "v0.1.0" becomes "0.1.0".
// Both sides of the compare are stripped, so the prefix cancels.
func stripV(s string) string {
	if strings.HasPrefix(s, "v") || strings.HasPrefix(s, "V") {
		return s[1:]
	}
	return s
}

// splitVer splits "major.minor.patch" into three integers, treating any absent
// or non-numeric component as 0. It returns exactly three components so compare
// can index it without bounds checks.
func splitVer(s string) [3]int {
	var out [3]int
	parts := strings.SplitN(s, ".", 3)
	for i := 0; i < len(parts); i++ {
		n, err := strconv.Atoi(strings.TrimSpace(parts[i]))
		if err == nil {
			out[i] = n
		}
	}
	return out
}
