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
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"garmin-sync/internal/config"
	"garmin-sync/internal/fitsel"
	"garmin-sync/internal/garmin"
	"garmin-sync/internal/state"
)

// exit codes
const (
	exitOK      = 0
	exitConfig  = 2
	exitGarmin  = 3
	exitUpload  = 4
	exitGeneral = 1
)

// test hooks: overridden by tests to point at the fake servers.
var (
	garminBaseURL = garmin.BaseURL
	fitselBaseURL = "https://ingest.fitsel.app/ingest/wh"
)

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

// mapGarminExit turns the two Garmin-side transport failures into their own
// exit codes (bad key, rate limit); everything else falls through to the
// general error code. See the README for the codes an athlete or a cron can
// act on.
func mapGarminExit(err error) int {
	if errors.Is(err, garmin.ErrBadKey) {
		return exitGarmin
	}
	return exitGeneral
}

func run(args []string) error {
	fs := flag.NewFlagSet("garmin-sync", flag.ContinueOnError)
	since := fs.String("since", "", "only sync activities on or after this date (YYYY-MM-DD)")
	dry := fs.Bool("dry-run", false, "list what would be uploaded, upload nothing")
	statePath := fs.String("state", "", "path to the state file (default: ~/.config/garmin-sync/state.json)")
	endpoint := fs.String("fitsel-endpoint", "https://ingest.fitsel.app/ingest/wh", "fitsel webhook base URL")
	if err := fs.Parse(args); err != nil {
		return err
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

	acts, err := gar.List(ctx, *since)
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
		if !accepted {
			return fmt.Errorf("uploading %s: fitsel did not accept it", itoa(a.ID))
		}
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
