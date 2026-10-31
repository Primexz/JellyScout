// Package cli connects the audit, API client, and output formats.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Primexz/jellyscout/internal/audit"
	"github.com/Primexz/jellyscout/internal/jellyfin"
	"github.com/Primexz/jellyscout/internal/report"
)

const usage = `JellyScout — find missing artwork and incomplete Jellyfin metadata.

Usage:
  jellyscout libraries [flags]
  jellyscout scan [flags]

Connection: --url, --api-key, --timeout (or JELLYFIN_URL / JELLYFIN_API_KEY).
Output: --format terminal|json. Save an additional JSON scan with --json <file>.
Use "jellyscout scan --help" for scan options and check names.
`

type repeatedIDs []string

func (ids *repeatedIDs) String() string { return strings.Join(*ids, ",") }
func (ids *repeatedIDs) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("library ID must not be empty")
	}
	*ids = append(*ids, strings.TrimSpace(value))
	return nil
}

// Run returns a process exit code and avoids process-wide state for testability.
// Diagnostic writes to stderr are best effort; report writes are checked.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		if _, err := io.WriteString(stdout, usage); err != nil {
			return 2
		}
		return 0
	}
	command := args[0]
	if command != "scan" && command != "libraries" {
		_, _ = fmt.Fprintf(stderr, "Unknown command %q. Use jellyscout --help.\n", command)
		return 2
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	// Apply environment values after parsing so flag usage never prints secrets
	// as defaults, including on malformed arguments.
	baseURL := fs.String("url", "", "Jellyfin server URL, including any base path (JELLYFIN_URL)")
	apiKey := fs.String("api-key", "", "Jellyfin API key (JELLYFIN_API_KEY)")
	timeout := fs.Duration("timeout", 30*time.Second, "Timeout for each HTTP request")
	format := fs.String("format", "terminal", "Output format: terminal or json (progress stays on stderr)")
	var libs repeatedIDs
	var includeSeasons, includeEpisodes, verifyImages, failOnIssues bool
	var checksValue, jsonPath string
	var concurrency int
	if command == "scan" {
		fs.Var(&libs, "library", "Scan this library ID; repeat to select multiple libraries (default: all)")
		fs.BoolVar(&includeSeasons, "include-seasons", false, "Also check season posters, titles, and numbering")
		fs.BoolVar(&includeEpisodes, "include-episodes", false, "Also check episode stills, titles, synopses, and numbering")
		fs.BoolVar(&verifyImages, "verify-images", false, "Fetch and decode referenced artwork; may populate server image caches")
		fs.BoolVar(&failOnIssues, "fail-on-issues", false, "Exit 1 when a completed scan has metadata findings")
		fs.StringVar(&checksValue, "checks", strings.Join(audit.DefaultChecks(), ","), "Comma-separated checks; restricts the applicable checks per item type")
		fs.StringVar(&jsonPath, "json", "", "Save an additional JSON report to this file")
		fs.IntVar(&concurrency, "concurrency", 4, "Maximum concurrent image requests with --verify-images")
	}
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: jellyscout %s [flags]\n\n", command)
		fs.PrintDefaults()
		if command == "scan" {
			_, _ = fmt.Fprintln(stderr, "\nMovie/show: primary-image, backdrop, title, overview, year, genres, provider-id.")
			_, _ = fmt.Fprintln(stderr, "Season: primary-image, title, numbering. Episode: primary-image, title, overview, numbering.")
		}
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "Unexpected positional arguments; put flags after the command.")
		return 2
	}
	if *format != "terminal" && *format != "json" {
		_, _ = fmt.Fprintln(stderr, "--format must be terminal or json")
		return 2
	}
	var urlSet, keySet bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "url":
			urlSet = true
		case "api-key":
			keySet = true
		}
	})
	if !urlSet {
		*baseURL = getenv("JELLYFIN_URL")
	}
	if !keySet {
		*apiKey = getenv("JELLYFIN_API_KEY")
	}
	var checks []string
	if command == "scan" {
		var err error
		checks, err = audit.ParseChecks(checksValue)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 2
		}
		if concurrency <= 0 {
			_, _ = fmt.Fprintln(stderr, "--concurrency must be positive")
			return 2
		}
	}
	client, err := jellyfin.New(*baseURL, *apiKey, *timeout)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	defer client.Close()
	server, err := client.Server(ctx)
	if err != nil {
		printAPIError(stderr, err)
		return 2
	}
	libraries, err := client.Libraries(ctx)
	if err != nil {
		printAPIError(stderr, err)
		return 2
	}
	if command == "libraries" {
		if *format == "json" {
			err = report.WriteLibrariesJSON(stdout, server, client.BaseURL(), libraries)
		} else {
			_, err = fmt.Fprintf(stdout, "Jellyfin URL: %s\n\n", client.BaseURL())
			if err == nil {
				err = report.WriteLibraries(stdout, libraries)
			}
		}
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "Write libraries:", err)
			return 2
		}
		return 0
	}
	selected, err := selectLibraries(libraries, libs)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	types := []string{"Movie", "Series"}
	if includeSeasons {
		types = append(types, "Season")
	}
	if includeEpisodes {
		types = append(types, "Episode")
	}
	r := audit.Report{Server: server, ServerURL: client.BaseURL(), VerifyImages: verifyImages, Complete: true, Checks: checks}
	seen := make(map[string]bool)
	for _, lib := range selected {
		_, _ = fmt.Fprintf(stderr, "Scanning library %s...\n", lib.ID)
		items, err := client.Items(ctx, lib.ID, types, verifyImages)
		if err != nil {
			printAPIError(stderr, err)
			return 2
		}
		for _, item := range items {
			if seen[item.ID] || !slices.Contains(types, item.Type) {
				continue
			}
			leaf := item.Type == "Movie" || item.Type == "Episode"
			if leaf && (item.IsPlaceHolder || strings.EqualFold(item.LocationType, "Virtual")) {
				continue
			}
			seen[item.ID] = true
			r.Results = append(r.Results, audit.Result{
				Library: lib, Item: item, URL: client.ItemURL(item.ID, server.ID),
				Findings: audit.Check(item, checks),
			})
		}
	}
	if verifyImages {
		if err := verify(ctx, client, &r, concurrency, stderr); err != nil {
			printAPIError(stderr, err)
			return 2
		}
	}
	if err := ctx.Err(); err != nil {
		_, _ = fmt.Fprintln(stderr, "Scan interrupted:", err)
		return 2
	}
	r.GeneratedAt = time.Now().UTC()
	audit.Sort(&r)
	if jsonPath != "" {
		if err := report.WriteJSONFile(jsonPath, r); err != nil {
			_, _ = fmt.Fprintln(stderr, "Write JSON report:", err)
			return 2
		}
		_, _ = fmt.Fprintln(stderr, "JSON report written to", jsonPath)
	}
	if *format == "json" {
		err = report.WriteJSON(stdout, r)
	} else {
		err = report.WriteTerminal(stdout, r)
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "Write report:", err)
		return 2
	}
	if !r.Complete {
		return 2
	}
	if failOnIssues {
		for _, result := range r.Results {
			if len(result.Findings) > 0 {
				return 1
			}
		}
	}
	return 0
}

func printAPIError(w io.Writer, err error) {
	if jellyfin.IsAuthError(err) {
		_, _ = fmt.Fprintln(w, "Jellyfin authentication/permission failure: check your server API key.", err)
	} else {
		_, _ = fmt.Fprintln(w, "Scan failed:", err)
	}
}

func selectLibraries(all []jellyfin.Library, ids []string) ([]jellyfin.Library, error) {
	if len(ids) == 0 {
		return all, nil
	}
	for _, id := range ids {
		if !slices.ContainsFunc(all, func(lib jellyfin.Library) bool { return lib.ID == id }) {
			return nil, fmt.Errorf("unknown library ID %q; run jellyscout libraries to list IDs", id)
		}
	}
	var selected []jellyfin.Library
	for _, lib := range all {
		if slices.Contains(ids, lib.ID) {
			selected = append(selected, lib)
		}
	}
	return selected, nil
}

type imageJob struct {
	result int
	ref    jellyfin.ImageRef
}

type imageOutcome struct {
	job     imageJob
	problem string
	err     error
}

func verify(ctx context.Context, client *jellyfin.Client, r *audit.Report, concurrency int, stderr io.Writer) error {
	var jobs []imageJob
	for i, result := range r.Results {
		for _, ref := range audit.ImageRefs(result.Item, r.Checks) {
			jobs = append(jobs, imageJob{i, ref})
		}
	}
	_, _ = fmt.Fprintf(stderr, "Verifying %d images...\n", len(jobs))
	if len(jobs) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	work := make(chan imageJob)
	outcomes := make(chan imageOutcome, min(concurrency, len(jobs)))
	var wg sync.WaitGroup
	wg.Go(func() {
		defer close(work)
		for _, job := range jobs {
			select {
			case work <- job:
			case <-ctx.Done():
				return
			}
		}
	})
	for range min(concurrency, len(jobs)) {
		wg.Go(func() {
			for job := range work {
				problem, err := client.VerifyImage(ctx, job.ref)
				outcomes <- imageOutcome{job, problem, err}
			}
		})
	}
	go func() { wg.Wait(); close(outcomes) }()
	var fatal error
	for outcome := range outcomes {
		if jellyfin.IsAuthError(outcome.err) {
			if fatal == nil {
				fatal = outcome.err
				cancel()
			}
			continue
		}
		result := &r.Results[outcome.job.result]
		if outcome.err != nil {
			r.Complete = false
			result.Errors = append(result.Errors, outcome.err.Error())
			_, _ = fmt.Fprintln(stderr, "Image verification:", outcome.err)
		} else if outcome.problem != "" {
			check := "primary-image"
			if outcome.job.ref.Type == "Backdrop" {
				check = "backdrop"
			}
			result.Findings = append(result.Findings, audit.Finding{Check: check, Message: outcome.problem})
		}
	}
	if fatal != nil {
		return fatal
	}
	return ctx.Err()
}
