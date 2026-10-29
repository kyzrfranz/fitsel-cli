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
	"strings"
	"time"

	"garmin-sync/internal/config"
	"garmin-sync/internal/fitsel"
	"garmin-sync/internal/garmin"
	"garmin-sync/internal/state"
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
)

// test hooks: overridden by tests to point at the fake servers.
var (
	garminBaseURL = garmin.BaseURL
	fitselBaseURL = "https://ingest.fitsel.app/ingest/wh"
)

// version is set at release time by GoReleaser via -ldflags "-X main.version=...".
var version = "dev"

// errPartialFailure wraps the run when at least one activity failed to sync but
// the run finished: it carries the count so main can print a one-line summary
// and mapGarminExit can give the partial run its own exit code.
var errPartialFailure = errors.New("partial: activity sync failed")

func main() {
	if err := run(os.Args[1:]); err != nil {
		exitError(err, os.Exit)
	}
}

// exitError prints the one-line error message and exits with the code mapped
// from the failure. It is factored out so the print-and-exit contract can be
// tested without os.Exit killing the test process (see TestErrorPrintedOnStderr).
func exitError(err error, exit func(int)) {
	fmt.Fprintln(os.Stderr, "error:", err)
	exit(mapGarminExit(err))
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

// mapGarminExit turns the transport failures into their own exit codes so an
// athlete (or a cron) can act on them (see the README). It handles both the
// Garmin-side failures and the fitsel upload failures; everything else falls
// through to the general error code.
func mapGarminExit(err error) int {
	switch {
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

	fs := flag.NewFlagSet("fitsel-client", flag.ContinueOnError)
	since := fs.String("since", "", "only sync activities on or after this date (YYYY-MM-DD)")
	to := fs.String("to", "", "only sync activities on or before this date (YYYY-MM-DD); omit for an open-ended upper bound")
	dry := fs.Bool("dry-run", false, "list what would be uploaded, upload nothing")
	statePath := fs.String("state", "", "path to the state file (default: ~/.config/garmin-sync/state.json)")
	endpoint := fs.String("fitsel-endpoint", "https://ingest.fitsel.app/ingest/wh", "fitsel webhook base URL")
	showVersion := fs.Bool("version", false, "print the version and exit")
	// SkipThreshold is how many per-activity failures before an activity is
	// marked skipped (no longer retried until --unskip). A run carries it as a
	// policy; the state file records only the per-activity count it counts
	// toward (per #132). Zero disables the skip entirely.
	skipThreshold := fs.Int("skip-threshold", 3, "mark an activity skipped after this many sync failures (0 to disable)")
	unskip := fs.Bool("unskip", false, "clear the skip on every activity so they are retried, then exit")
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
	up := fitsel.New(*endpoint, creds.FitselPasskey)

	ctx := context.Background()
	if _, err := gar.Authorize(ctx); err != nil {
		return fmt.Errorf("authenticating with Garmin: %w", err)
	}

	if *since == "" {
		// First run: without --since, nothing is listed yet. Later stories
		// default this to a sensible start date; here the athlete must pass
		// --since on the first run.
		return errors.New("first run: pass --since YYYY-MM-DD to set the start date")
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

	synced, failed := 0, 0
	var partial bool
	for _, a := range pending {
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
				return fmt.Errorf("saving state: %w", err)
			}
			failed++
			partial = true
			continue
		}
		accepted, err := up.Upload(fit)
		if err != nil {
			// An upload failure is run-wide: a bad passkey (401), a missing
			// webhook (404), or fitsel giving up after retries (500) will
			// fail every activity, so stop the run with its own exit code.
			if isRunWideUpload(err) {
				return err
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
				return fmt.Errorf("saving state: %w", err)
			}
			failed++
			partial = true
			continue
		}
		_ = accepted // Upload returns nil only on 202; a nil error means success.
		synced++
		st.MarkDone(id)
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
