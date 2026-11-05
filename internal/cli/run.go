// Package cli connects the audit, API client, and output formats.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Primexz/jellyscout/internal/audit"
	"github.com/Primexz/jellyscout/internal/jellyfin"
	"github.com/Primexz/jellyscout/internal/report"
	"github.com/Primexz/jellyscout/internal/scan"
	"github.com/Primexz/jellyscout/internal/tui"
)

const usage = `JellyScout — find missing artwork and incomplete Jellyfin metadata.

Usage:
  jellyscout libraries [flags]
  jellyscout scan [flags]
  jellyscout tui [flags]

Connection: --url, --api-key, --timeout (or JELLYFIN_URL / JELLYFIN_API_KEY).
Output: --format terminal|json (scan/libraries). Save JSON with --json <file> (scan/tui).
Interactive: tui requires terminal stdin and stdout; --repair-timeout defaults to 2m.
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
	return RunWithInput(ctx, args, strings.NewReader(""), stdout, stderr, getenv)
}

// RunWithInput runs a command using explicit input and output streams.
// Interactive mode requires terminal input and output.
func RunWithInput(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		if _, err := io.WriteString(stdout, usage); err != nil {
			return 2
		}
		return 0
	}
	command := args[0]
	if command != "scan" && command != "libraries" && command != "tui" {
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
	format := "terminal"
	if command != "tui" {
		fs.StringVar(&format, "format", "terminal", "Output format: terminal or json (progress stays on stderr)")
	}
	var repairTimeout time.Duration
	if command == "tui" {
		fs.DurationVar(&repairTimeout, "repair-timeout", 2*time.Minute, "Maximum wait for each queued repair before continuing")
	}
	var libs repeatedIDs
	var includeSeasons, includeEpisodes, verifyImages, failOnIssues bool
	var checksValue, jsonPath string
	var concurrency int
	if command == "scan" || command == "tui" {
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
		if command == "scan" || command == "tui" {
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
	if format != "terminal" && format != "json" {
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
	if command == "scan" || command == "tui" {
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
	if command == "tui" {
		if repairTimeout <= 0 {
			_, _ = fmt.Fprintln(stderr, "--repair-timeout must be positive")
			return 2
		}
		if !tui.IsTerminal(stdin, stdout) {
			_, _ = fmt.Fprintln(stderr, "jellyscout tui requires terminal stdin and stdout")
			return 2
		}
	}
	client, err := jellyfin.New(*baseURL, *apiKey, *timeout)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	defer client.Close()
	if command == "libraries" {
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

		if format == "json" {
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
	opts := scan.Options{
		LibraryIDs: libs, IncludeSeasons: includeSeasons, IncludeEpisodes: includeEpisodes,
		VerifyImages: verifyImages, Checks: checks, Concurrency: concurrency,
	}
	var r audit.Report
	if command == "tui" {
		r, err = tui.Run(ctx, stdin, stdout, client, tui.Options{Scan: opts, RepairTimeout: repairTimeout})
	} else {
		r, err = scan.Run(ctx, client, opts, func(message string) {
			_, _ = fmt.Fprintln(stderr, message)
		})
	}
	if err != nil {
		printAPIError(stderr, err)
		return 2
	}
	if err := ctx.Err(); err != nil {
		_, _ = fmt.Fprintln(stderr, "Scan interrupted:", err)
		return 2
	}
	if jsonPath != "" {
		if err := report.WriteJSONFile(jsonPath, r); err != nil {
			_, _ = fmt.Fprintln(stderr, "Write JSON report:", err)
			return 2
		}
		_, _ = fmt.Fprintln(stderr, "JSON report written to", jsonPath)
	}
	if command != "tui" {
		if format == "json" {
			err = report.WriteJSON(stdout, r)
		} else {
			err = report.WriteTerminal(stdout, r)
		}
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "Write report:", err)
		return 2
	}
	if !r.Complete {
		return 2
	}
	for _, result := range r.Results {
		if len(result.Errors) > 0 {
			return 2
		}
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
