// Command fitsel-client is a client the athlete runs on their own machine: each
// run it uses the athlete's Garmin Personal API key to fetch the activities
// they have not synced yet as FIT files, and uploads each one to the athlete's
// fitsel upload webhook.
//
// It is a client, not a hosted service: the athlete's Garmin key never leaves
// the athlete's machine (Garmin's Personal API is "not intended for commercial
// applications serving multiple end users").
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kyzrfranz/fitsel-cli/internal/checker"
	"github.com/kyzrfranz/fitsel-cli/internal/config"
	"github.com/kyzrfranz/fitsel-cli/internal/fitsel"
	"github.com/kyzrfranz/fitsel-cli/internal/garmin"
	"github.com/kyzrfranz/fitsel-cli/internal/state"
)

// exit codes
const (
	exitOK          = 0
	exitConfig      = 2
	exitGarmin      = 3
	exitRateLimited = 5
	exitGeneral     = 1

	// fitsel upload errors (each its own code, per #85)
	exitBadSignature    = 6
	exitWebhookMissing  = 7
	exitUploadExhausted = 8

	// partialFailure is returned when a run finished but at least one activity
	// failed to sync (its download or upload errored): the run completed, but
	// the athlete should re-run to retry the ones that failed. It is distinct
	// from the run-wide codes above, which stop the whole run before any
	// activity is attempted.
	exitPartialFailure = 9

	// exitInterrupted is returned when the run was stopped by a signal
	// (Ctrl-C / SIGTERM): the athlete asked it to stop mid-run. It is distinct
	// from partialFailure (a run that finished with a failure) and from the
	// run-wide codes (which abort before any activity).
	exitInterrupted = 4
)

// test hooks: overridden by tests to point at the fake servers.
var (
	garminBaseURL = garmin.BaseURL
	fitselBaseURL = "https://ingest.fitsel.app/ingest/wh"

	// The clients' retry/back-off delays default to their production values
	// (2s upload / 30s back-off); tests point them at ~0 so the suite does not
	// sleep real seconds. See the garmin and fitsel packages.
)

// version is set at release time by GoReleaser via -ldflags "-X main.version=...".
// The default carries a "-dev" suffix so a local build (go run .) is recognised
// as a dev build and skips the update check (see internal/checker); a release
// overrides it with a plain semver like "1.2.0".
var version = "0.0.0-dev"

// githubBaseURL overrides the GitHub API base the update checker queries. It is
// empty in production (the checker uses the real GitHub); tests point it at an
// httptest fake so the check makes no real network call.
var githubBaseURL = ""

// errPartialFailure wraps the run when at least one activity failed to sync but
// the run finished: it carries the count so main can print a one-line summary
// and mapExitCode can give the partial run its own exit code.
var errPartialFailure = errors.New("partial: activity sync failed")

// errInterrupted wraps the run when it was stopped by a signal (Ctrl-C /
// SIGTERM) mid-run: the athlete asked it to stop, so it is reported with its
// own exit code (exitInterrupted) and is distinct from a partial failure.
var errInterrupted = errors.New("sync interrupted")

func main() {
	err := run(os.Args[1:])
	if err != nil {
		reportError(err, os.Exit)
	}
}

// reportError prints the run's failure and exits with the mapped code. An
// interrupted run (a Ctrl-C / SIGTERM the athlete asked for) is reported with
// its own one-line message and its own exit code, not the generic `error:`
// line — a stop is the athlete asking to pause, not a failure. Everything else
// falls through to exitError.
func reportError(err error, exit func(int)) {
	if errors.Is(err, errInterrupted) {
		fmt.Fprintln(os.Stderr, "sync interrupted")
		exit(exitInterrupted)
		return
	}
	exitError(err, exit)
}

// exitError prints the one-line error message and exits with the code mapped
// from the failure. It is factored out so the print-and-exit contract can be
// tested without os.Exit killing the test process (see TestErrorPrintedOnStderr).
func exitError(err error, exit func(int)) {
	fmt.Fprintln(os.Stderr, "error:", err)
	exit(mapExitCode(err))
}

// isRunWideUpload reports whether an upload failure is run-wide: it will fail
// every activity, so the run must abort rather than skip one activity. A bad
// passkey (401), a missing webhook (404), or fitsel giving up after retries
// (500) are all persistent; a malformed upload (400) is a single bad activity
// and is skipped.
func isRunWideUpload(err error) bool {
	return errors.Is(err, fitsel.ErrBadSignature) ||
		errors.Is(err, fitsel.ErrNotFound) ||
		errors.Is(err, fitsel.ErrUploadExhausted)
}

// mapExitCode turns failures into their own exit codes so an athlete (or a
// cron) can act on them (see the README). It handles config and credential
// failures, the Garmin-side failures and the fitsel upload failures; everything
// else falls through to the general error code.
func mapExitCode(err error) int {
	switch {
	case errors.Is(err, config.ErrMissingCredentials):
		return exitConfig
	case errors.Is(err, config.ErrConfigUnreadable):
		return exitConfig
	case errors.Is(err, garmin.ErrBadKey):
		return exitGarmin
	case errors.Is(err, garmin.ErrRateLimited):
		return exitRateLimited
	case errors.Is(err, fitsel.ErrUploadExhausted):
		return exitUploadExhausted
	case errors.Is(err, fitsel.ErrBadSignature):
		return exitBadSignature
	case errors.Is(err, fitsel.ErrNotFound):
		return exitWebhookMissing
	case errors.Is(err, errPartialFailure):
		return exitPartialFailure
	case errors.Is(err, errInterrupted):
		return exitInterrupted
	default:
		return exitGeneral
	}
}

func run(args []string) error {
	// `init` is a distinct setup subcommand: it must never reach the sync path,
	// so it is dispatched before any flag parsing below.
	if len(args) > 0 && args[0] == "init" {
		return initCommand(os.Stdin, os.Stdout, args[1:])
	}
	if len(args) > 0 && args[0] == "doctor" {
		return runDoctor(os.Stdin, os.Stdout, args[1:])
	}
	if len(args) > 0 && args[0] == "status" {
		return runStatus(os.Stdin, os.Stdout, args[1:])
	}

	// A Ctrl-C (SIGINT) or SIGTERM stops the run cleanly: the derived context
	// reaches the sync loop, so an in-flight download or upload returns instead
	// of running to completion, and no activity that was not already uploaded
	// is marked synced. stop releases the signal handler when the run ends.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return runWithCtx(ctx, args)
}

// runWithCtx runs a sync with an already-derived context. It is the testable
// core: a test drives cancellation by cancelling ctx directly, rather than
// sending a real signal. run() wraps it with signal.NotifyContext so a Ctrl-C
// or SIGTERM cancels the same context.
func runWithCtx(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fitsel-client", flag.ContinueOnError)
	since := fs.String("since", "", "only sync activities on or after this date (YYYY-MM-DD)")
	to := fs.String("to", "", "only sync activities on or before this date (YYYY-MM-DD); omit for an open-ended upper bound")
	dry := fs.Bool("dry-run", false, "list what would be uploaded, upload nothing")
	statePath := fs.String("state", "", "path to the state file (default: "+state.Path()+")")
	endpoint := fs.String("fitsel-endpoint", "https://ingest.fitsel.app/ingest/wh", "fitsel webhook base URL; the upload goes to <base>/<FITSEL_TOKEN>")
	showVersion := fs.Bool("version", false, "print the version and exit")
	// SkipThreshold is how many per-activity failures before an activity is
	// marked skipped (no longer retried until --unskip). A run carries it as a
	// policy; the state file records only the per-activity count it counts
	// toward (per #132). Zero disables the skip entirely.
	skipThreshold := fs.Int("skip-threshold", 3, "mark an activity skipped after this many sync failures (0 to disable)")
	unskip := fs.Bool("unskip", false, "clear the skip on every activity so they are retried, then exit")
	noUpdateCheck := fs.Bool("no-update-check", false, "skip the daily update check (or set FITSEL_NO_UPDATE_CHECK=1)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println("fitsel-client", version)
		return nil
	}

	if _, err := parseDate(*since); err != nil {
		return fmt.Errorf("--since: %w", err)
	}
	if _, err := parseDate(*to); err != nil {
		return fmt.Errorf("--to: %w", err)
	}

	creds, err := config.Load()
	if err != nil {
		return err
	}

	st, err := state.Load(*statePath)
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}
	// Best-effort daily update check: prints a one-line upgrade hint when a
	// newer version exists. It is silent on every failure and throttled to once
	// per day, so a slow or unreachable GitHub can never block the sync. It is
	// skipped for a dev build or when the athlete opts out.
	runUpdateCheck(*noUpdateCheck)

	// A run carries the skip policy; the state defaults it to 3, and the flag
	// overrides it. The flag is read with fs.Visit (not its default value) so
	// 0 is honoured as "disable" rather than falling back to the default: the
	// default only applies when the flag is left unset.
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "skip-threshold" {
			st.SkipThreshold = *skipThreshold
		}
	})
	// --unskip clears the skip on every activity so they are retried, then
	// exits without a sync run: it is a maintenance action, not a sync.
	if *unskip {
		for _, id := range st.Skips() {
			st.Unskip(id)
		}
		if err := st.Save(); err != nil {
			return fmt.Errorf("saving state: %w", err)
		}
		fmt.Println("cleared skip on all activities")
		return nil
	}

	gar := garmin.New(creds.GarminKey)
	gar.SetBaseURL(garminBaseURL)
	gar.RetryAfter = garmin.RetryAfterDelay
	up := fitsel.New(*endpoint, creds.FitselToken, creds.FitselPasskey)
	up.UploadDelay = fitsel.UploadDelayDelay

	if _, err := gar.Authorize(ctx); err != nil {
		return fmt.Errorf("authenticating with Garmin: %w", err)
	}

	if *since == "" {
		// Not the first run with an explicit --since: the stored sync point
		// decides the list start. An empty point (a brand-new state, or a
		// pre-#127 file that migrated in place with no known point) is treated
		// exactly like a first run: require --since rather than listing all
		// history since the start of time.
		start := st.StartTime()
		if start == "" {
			return errors.New("first run: pass --since YYYY-MM-DD to set the start date")
		}
		*since = parseDateOrEarlier(start)
	}

	acts, err := gar.List(ctx, *since, *to)
	if err != nil {
		return fmt.Errorf("listing activities: %w", err)
	}

	// Filter to those not already synced, preserving the list order (newest
	// first).
	var pending []garmin.Activity
	for _, a := range acts {
		if !st.Done(itoa(a.ID)) {
			pending = append(pending, a)
		}
	}

	if len(pending) == 0 {
		fmt.Println("nothing to sync — up to date")
		return nil
	}

	if *dry {
		fmt.Printf("%d activity(ies) would be uploaded:\n", len(pending))
		for _, a := range pending {
			fmt.Printf("  - %s (%s)\n", a.Name, itoa(a.ID))
		}
		return nil
	}

	synced, failed, partial, err := syncActivities(ctx, gar, up, st, pending)
	if err != nil {
		return err
	}
	// Lock the sync point after a real sync of at least one activity: the
	// newest activity's start date seen this run becomes the point a later
	// --since-less run lists from. Never lower it. A dry-run never reaches
	// here, so a preview never skips an upload you then make.
	if len(pending) > 0 {
		var newest string
		for _, a := range pending {
			if d := a.StartGMT[:10]; newest == "" || d > newest {
				newest = d
			}
		}
		st.AdvanceStartIfLater(newest)
		if err := st.Save(); err != nil {
			return fmt.Errorf("saving state: %w", err)
		}
	}
	// List any activities the run left skipped (they failed too many times), so
	// the athlete knows what is no longer being retried and how to revive them.
	if skipped := st.Skips(); len(skipped) > 0 {
		fmt.Printf("skipped %d activity(ies) (failed too many times): %s\n", len(skipped), strings.Join(skipped, ", "))
	}
	if partial {
		// The run finished but some activities failed. Print the summary and
		// return a partial-failure error so main maps it to its own non-zero
		// exit code; the run completed, so this is distinct from the run-wide
		// codes that abort before any activity is attempted.
		fmt.Printf("done: %d synced, %d failed this run\n", synced, failed)
		return fmt.Errorf("%w: %d activity(ies) failed to sync", errPartialFailure, failed)
	}
	fmt.Printf("done: %d synced, %d failed this run\n", synced, failed)
	return nil
}

// runUpdateCheck performs the best-effort daily update check and prints a
// one-line upgrade hint to stdout when a newer version exists. It is silent on
// every failure (connection error, timeout, non-2xx, malformed body) and on the
// skip paths (dev build, throttled, or opted out), so it can never block the
// sync. The opt-out is the --no-update-check flag or FITSEL_NO_UPDATE_CHECK=1.
func runUpdateCheck(flagDisabled bool) {
	if flagDisabled || os.Getenv("FITSEL_NO_UPDATE_CHECK") == "1" {
		return
	}
	checker.SetVersion(version)
	if githubBaseURL != "" {
		checker.Base = githubBaseURL
	}
	hint, err := checker.Run(checker.Options{})
	if err != nil {
		return // skipped (dev build, throttled, opted out) or failed: stay silent.
	}
	if hint == nil {
		return // up to date or older: nothing to say.
	}
	fmt.Println(hint.Text)
}

// syncActivities downloads and uploads every pending activity in order, marking
// each one done only after its upload succeeds. It threads ctx through every
// download and upload so a cancelled context (a Ctrl-C) stops the loop promptly,
// and returns a partial-failure error if any activity failed while the rest of
// the run completed.
//
// On cancellation it returns errInterrupted without marking the in-flight
// activity: every activity uploaded before the stop has already persisted its
// success via Save, so nothing that was synced is lost, and nothing past the
// last success is marked synced.
func syncActivities(ctx context.Context, gar *garmin.Client, up *fitsel.Client, st *state.State, pending []garmin.Activity) (synced, failed int, partial bool, err error) {
	for _, a := range pending {
		// A stop mid-run: bail before doing any more work, leaving this
		// activity (and everything after it) unmarked.
		if err := ctx.Err(); err != nil {
			return 0, 0, false, errInterrupted
		}
		id := itoa(a.ID)
		// An activity that has failed SkipThreshold times is marked skipped:
		// leave it alone until it is unskipped, and report it in the summary
		// rather than retrying it every run.
		if st.Skipped(id) {
			fmt.Printf("skipping %s (%s): failed too many times, unskip to retry\n", a.Name, id)
			continue
		}
		fmt.Printf("syncing %s (%s)...\n", a.Name, id)
		fit, err := gar.Download(ctx, a.ID)
		if err != nil {
			// A stop mid-run: do not count it or mark it; leave it pending.
			if err := ctx.Err(); err != nil {
				return 0, 0, false, errInterrupted
			}
			// A per-activity download failure (a single activity's FIT is
			// missing or the download dropped): log it, count it, leave the
			// activity pending, and move on. The rest of the run still syncs.
			// If it has now failed too many times, it is marked skipped.
			fmt.Fprintf(os.Stderr, "error: skipping %s: %v\n", id, err)
			if st.Fail(id) {
				fmt.Fprintf(os.Stderr, "error: %s has failed %d times; it is now skipped. Use --unskip to retry it.\n", id, st.SkipThreshold)
			}
			// Persist the incremented count (and any skip) so it survives
			// across runs; a run that only saved on success would lose it.
			if err := st.Save(); err != nil {
				return 0, 0, false, fmt.Errorf("saving state: %w", err)
			}
			failed++
			partial = true
			continue
		}
		accepted, err := up.Upload(ctx, fit)
		if err != nil {
			// A stop mid-run: do not count it or mark it; leave it pending.
			if err := ctx.Err(); err != nil {
				return 0, 0, false, errInterrupted
			}
			// An upload failure is run-wide: a bad passkey (401), a missing
			// webhook (404), or fitsel giving up after retries (500) will
			// fail every activity, so stop the run with its own exit code.
			if isRunWideUpload(err) {
				return 0, 0, false, err
			}
			// A per-activity upload failure (e.g. a malformed upload the
			// endpoint rejects): log it, count it, leave the activity pending,
			// and move on. The rest of the run still syncs.
			fmt.Fprintf(os.Stderr, "error: skipping %s: %v\n", id, err)
			if st.Fail(id) {
				fmt.Fprintf(os.Stderr, "error: %s has failed %d times; it is now skipped. Use --unskip to retry it.\n", id, st.SkipThreshold)
			}
			// Persist the incremented count (and any skip) so it survives
			// across runs; a run that only saved on success would lose it.
			if err := st.Save(); err != nil {
				return 0, 0, false, fmt.Errorf("saving state: %w", err)
			}
			failed++
			partial = true
			continue
		}
		_ = accepted // Upload returns nil only on 202; a nil error means success.
		synced++
		// Persist immediately: an activity uploaded before the stop stays
		// saved even if the run is stopped on the next activity. MarkDone also
		// records the activity's name and start, and stamps the run-level
		// last-synced time on the first activity of the run, so `status` can
		// report these offline.
		st.MarkDone(id, a.Name, a.StartGMT)
		if err := st.Save(); err != nil {
			return 0, 0, false, fmt.Errorf("saving state: %w", err)
		}
	}
	return synced, failed, partial, nil
}

// itoa formats an activity id as a string key for the state file.
func itoa(id int64) string {
	return fmt.Sprintf("%d", id)
}

// parseDate parses a YYYY-MM-DD date, returning a calendar-valid date (so it
// rejects e.g. 2026-02-30, not just the shape). An empty string is valid: it
// means "no bound" for --to, and --since's own first-run check covers the empty
// case, so callers that treat empty as a missing bound see no error here.
func parseDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, errors.New("want a calendar date YYYY-MM-DD")
	}
	return t, nil
}

// parseDateOrEarlier returns the start date a --since-less run lists from: the
// stored sync point minus a 2-day overlap, so an activity synced at the tail of
// the last window is not skipped. The overlap is a safety margin, not a bound:
// the client still skips anything already synced, so a wider window only costs
// one extra page, never a missed upload.
func parseDateOrEarlier(start string) string {
	t, err := time.Parse("2006-01-02", start)
	if err != nil {
		return start
	}
	return t.AddDate(0, 0, -2).Format("2006-01-02")
}

// initCommand is the interactive setup subcommand: it walks the athlete through
// entering the three secrets and stores them in the home-dir config, instead of
// editing the JSON by hand or setting env vars. It never starts a sync run.
//
// It prompts, in order, for the Garmin Personal API key, the fitsel webhook
// token and the fitsel passkey, each read from in as one line with the trailing
// newline trimmed. The three secrets are the only thing stored: there is no
// endpoint prompt, so the static fitsel webhook URL from the pitch contract
// stays as the tool's default.
//
// It takes its input and output as arguments so tests can feed stdin and read
// the prompts in-process, with no real network and no real terminal.
func initCommand(in io.Reader, out io.Writer, args []string) error {
	// Any extra args are ignored: init takes no flags or endpoint.
	reader := bufio.NewReader(in)

	prompt := func(label string) (string, error) {
		if _, err := fmt.Fprintf(out, "%s: ", label); err != nil {
			return "", err
		}
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		return trimNewline(line), nil
	}

	garminKey, err := prompt("Garmin Personal API key")
	if err != nil {
		return fmt.Errorf("reading your Garmin key: %w", err)
	}
	fitselToken, err := prompt("fitsel webhook token")
	if err != nil {
		return fmt.Errorf("reading your fitsel webhook token: %w", err)
	}
	fitselPasskey, err := prompt("fitsel passkey")
	if err != nil {
		return fmt.Errorf("reading your fitsel passkey: %w", err)
	}

	// Require all three before writing anything: a partial config would be a
	// broken setup the athlete would have to fix by hand.
	if garminKey == "" || fitselToken == "" || fitselPasskey == "" {
		return errors.New("all three values are required; nothing was saved")
	}

	creds := config.Credentials{
		GarminKey:     garminKey,
		FitselToken:   fitselToken,
		FitselPasskey: fitselPasskey,
	}
	if err := creds.Save(); err != nil {
		return fmt.Errorf("saving credentials: %w", err)
	}

	if _, err := fmt.Fprintln(out, "Credentials saved to "+config.Path()); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, "Run 'fitsel-client doctor' to verify this setup."); err != nil {
		return err
	}
	return nil
}

// trimNewline drops a single trailing newline (and a following carriage return
// from CRLF input) without touching any other character, so a secret is stored
// exactly as typed.
func trimNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		s = strings.TrimSuffix(s, "\n")
		s = strings.TrimSuffix(s, "\r")
	}
	return s
}

// runDoctor is the `fitsel-client doctor` subcommand: it verifies the local
// setup and prints one ✓/✗ line per check, exiting 0 only if every check
// passes. It contacts no fitsel endpoint — structurally, it never builds a
// fitsel client or makes any fitel call — so the "no fitsel contact" property
// holds by construction. The only network call it makes is to Garmin's token
// endpoint, which is the point of check 3.
//
// It takes its input and output as arguments so tests can read the report
// in-process, with no real network and no real terminal. It never prints a
// secret value: it reports only the presence or absence of each key, never the
// key itself, and never echoes any field of the loaded credentials.
func runDoctor(in io.Reader, out io.Writer, args []string) error {
	// Doctor takes one flag, mirroring the sync -state flag's default.
	fs := flag.NewFlagSet("fitsel-client doctor", flag.ContinueOnError)
	statePath := fs.String("state", "", "path to the state file (default: "+state.Path()+")")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// An empty -state resolves to the default path, so a doctor run checks the
	// same file a sync would use.
	if *statePath == "" {
		*statePath = state.Path()
	}

	// All checks share one pass over the output: each returns a line and a
	// boolean, so the report stays a flat list and the exit code is "all
	// passed". A check that fails prints its ✗ line and marks the run failed,
	// but the report still lists the rest so the athlete sees everything at
	// once.
	type line struct {
		text string
		ok   bool
	}
	var lines []line
	fail := func(format string, a ...any) line {
		return line{fmt.Sprintf(format, a...), false}
	}
	ok := func(format string, a ...any) line {
		return line{fmt.Sprintf(format, a...), true}
	}

	// Check 1 — config found + mode 0600. Stat config.Path() directly:
	// config.Load() writes a placeholder and errors on missing keys, so a
	// fresh machine would create the file and pass by accident. We stat first,
	// before any Load, so a missing file is a clean ✗.
	fi, statErr := os.Stat(config.Path())
	if statErr != nil {
		lines = append(lines, fail("✗ config file not found at %s", config.Path()))
	} else if fi.Mode().Perm()&0o077 != 0 {
		lines = append(lines, fail("✗ config file %s is not mode 0600 (got %o)", config.Path(), fi.Mode().Perm()))
	} else {
		lines = append(lines, ok("✓ config file %s is present and mode %o", config.Path(), fi.Mode().Perm()))
	}

	// Check 2 — three secrets present. Load once; it also supplies the creds
	// for check 3. On success all three are present. On the missing-keys error
	// we report which key(s) are absent (named in the error, never their
	// value). On an unreadable/malformed config we report that reason.
	creds, loadErr := config.Load()
	if loadErr != nil {
		switch {
		case errors.Is(loadErr, config.ErrMissingCredentials):
			lines = append(lines, fail("✗ missing credentials: %v", loadErr))
		case errors.Is(loadErr, config.ErrConfigUnreadable):
			lines = append(lines, fail("✗ config present but unreadable: %v", loadErr))
		default:
			lines = append(lines, fail("✗ could not load credentials: %v", loadErr))
		}
	} else {
		lines = append(lines, ok("✓ all three secrets present"))
	}

	// Check 3 — Garmin key valid. Only if we could load creds and have a key:
	// exchange it for an access token via the existing client. This is the only
	// network call, and it is Garmin's token endpoint, which must be reached.
	if loadErr == nil && creds.GarminKey != "" {
		gar := garmin.New(creds.GarminKey)
		gar.SetBaseURL(garminBaseURL)
		if _, err := gar.Authorize(context.Background()); err != nil {
			if errors.Is(err, garmin.ErrBadKey) {
				lines = append(lines, fail("✗ Garmin key rejected (invalid key)"))
			} else {
				lines = append(lines, fail("✗ could not verify Garmin key: %v", err))
			}
		} else {
			lines = append(lines, ok("✓ Garmin key exchanges for an access token"))
		}
	}

	// Check 4 — state file present and readable. Load("") succeeds for a
	// missing file, so stat the resolved path first; a missing file is a ✗.
	if _, err := os.Stat(*statePath); err != nil {
		lines = append(lines, fail("✗ state file not found at %s", *statePath))
	} else if _, err := state.Load(*statePath); err != nil {
		lines = append(lines, fail("✗ state file %s present but unreadable: %v", *statePath, err))
	} else {
		lines = append(lines, ok("✓ state file %s is present and readable", *statePath))
	}

	// The fitsel check is not ✓/✗: there is no endpoint to verify the token or
	// passkey without uploading, so doctor prints the fixed line instead and
	// never builds a fitsel client or touches a fitsel endpoint.
	if _, err := fmt.Fprintln(out, "fitsel credentials can only be verified by a real sync"); err != nil {
		return err
	}

	// Print the report in order, then decide the exit code. Every line printed
	// to out is a check result or the fixed fitsel line — no secret value,
	// since every line reports only presence/absence or a resolved path.
	for _, l := range lines {
		fmt.Fprintln(out, l.text)
		if !l.ok {
			// A single unwrapped general error so main maps the run to its own
			// non-zero exit code (any failure of the health report is general,
			// not a specific transport failure).
			return errors.New("doctor: setup incomplete, fix the checks above")
		}
	}
	return nil
}

// runStatus is the `fitsel-client status` subcommand: an offline health check
// of the last syncs. With no flags it prints, exits 0, and touches NO network —
// it builds no gar client at all, so the newest synced activity's name/date and
// the last synced time come purely from the persisted state. `status --pending`
// is the only mode that touches the network: it mirrors --dry-run, listing what
// a sync would upload (what Garmin serves that is not already done), uploading
// nothing and leaving state byte-for-byte unchanged.
//
// It never prints a secret value: it reports the config file *path* only, never
// its contents or keys, mirroring doctor's no-secret discipline.
//
// It takes its input and output as arguments so tests can read the report
// in-process, with no real network and no real terminal.
func runStatus(in io.Reader, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("fitsel-client status", flag.ContinueOnError)
	statePath := fs.String("state", "", "path to the state file (default: "+state.Path()+")")
	pending := fs.Bool("pending", false, "list what a sync would upload, upload nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *statePath == "" {
		*statePath = state.Path()
	}

	// The report is the same shape whether or not --pending is given, so build
	// it once. It never loads the config's contents or keys: only the config
	// *path* is printed (matching doctor's no-secret rule), so we stat the path
	// to report it and load the state to report sync history.
	//
	// The config path is reported even when the config is unreadable: the path
	// is not a secret, and the athlete needs to know where their config is.
	// Its contents are never printed.

	// Load the state to report sync history. A missing state file is an empty
	// state, so "never synced yet" renders cleanly.
	st, err := state.Load(*statePath)
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}

	// Print the offline report: config path, state path, last synced, newest
	// synced activity, and synced count. No secret value — only paths.
	fmt.Fprintf(out, "config: %s\n", config.Path())
	fmt.Fprintf(out, "state: %s\n", *statePath)
	if st.LastSyncedAt().IsZero() {
		fmt.Fprintln(out, "last synced: never synced yet")
	} else {
		fmt.Fprintf(out, "last synced: %s\n", st.LastSyncedAt().Format("2006-01-02 15:04:05"))
	}
	if id, meta := st.NewestSynced(); id != "" {
		fmt.Fprintf(out, "newest synced activity: %s (%s)\n", meta.Name, meta.Start)
	} else {
		fmt.Fprintln(out, "newest synced activity: none")
	}
	fmt.Fprintf(out, "activities synced so far: %d\n", st.SyncedCount())

	// --pending is the only mode that touches the network. It mirrors --dry-run
	// exactly: build a gar client, list what would be uploaded, upload nothing.
	// It does NOT build a fitsel client, so no upload is sent.
	if !*pending {
		return nil
	}

	// --pending needs credentials to authenticate with Garmin. Load them; a
	// missing config is a clean error, not a crash. The secrets are never
	// printed — only used to build the gar client.
	creds, err := config.Load()
	if err != nil {
		return err
	}

	gar := garmin.New(creds.GarminKey)
	gar.SetBaseURL(garminBaseURL)
	gar.RetryAfter = garmin.RetryAfterDelay
	if _, err := gar.Authorize(context.Background()); err != nil {
		return fmt.Errorf("authenticating with Garmin: %w", err)
	}

	// List everything Garmin serves that is not already done, with no lower
	// bound: a health check wants to see everything pending, not just what a
	// --since window would have synced.
	acts, err := gar.List(context.Background(), "", "")
	if err != nil {
		return fmt.Errorf("listing activities: %w", err)
	}

	var pendingActs []garmin.Activity
	for _, a := range acts {
		if !st.Done(itoa(a.ID)) {
			pendingActs = append(pendingActs, a)
		}
	}

	fmt.Fprintln(out, "pending:")
	if len(pendingActs) == 0 {
		fmt.Fprintln(out, "  nothing to sync — up to date")
		return nil
	}
	for _, a := range pendingActs {
		fmt.Fprintf(out, "  - %s (%s)\n", a.Name, itoa(a.ID))
	}
	// State is loaded but never Saved: --pending uploads nothing and changes
	// nothing, so the state file is byte-for-byte unchanged.
	return nil
}
