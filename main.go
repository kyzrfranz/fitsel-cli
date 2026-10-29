// Command garmin-sync is a client the athlete runs on their own machine: each
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
	exitUpload      = 4
	exitRateLimited = 5
	exitGeneral     = 1

	// fitsel upload errors (each its own code, per #85)
	exitBadSignature    = 6
	exitWebhookMissing  = 7
	exitUploadExhausted = 8
)

// test hooks: overridden by tests to point at the fake servers.
var (
	garminBaseURL = garmin.BaseURL
	fitselBaseURL = "https://ingest.fitsel.app/ingest/wh"
)

// version is set at release time by GoReleaser via -ldflags "-X main.version=...".
var version = "dev"

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
	case errors.Is(err, fitsel.ErrBadRequest):
		return exitUpload
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

	for _, a := range pending {
		fmt.Printf("syncing %s (%s)...\n", a.Name, itoa(a.ID))
		fit, err := gar.Download(ctx, a.ID)
		if err != nil {
			return fmt.Errorf("downloading %s: %w", itoa(a.ID), err)
		}
		accepted, err := up.Upload(fit)
		if err != nil {
			return fmt.Errorf("uploading %s: %w", itoa(a.ID), err)
		}
		_ = accepted // Upload returns nil only on 202; a nil error means success.
		st.MarkDone(itoa(a.ID))
		if err := st.Save(); err != nil {
			return fmt.Errorf("saving state: %w", err)
		}
	}
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
