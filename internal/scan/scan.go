// Package scan shares metadata scanning and image verification between clients.
package scan

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Primexz/jellyscout/internal/audit"
	"github.com/Primexz/jellyscout/internal/jellyfin"
)

// Options selects the library scope and checks for a scan.
type Options struct {
	LibraryIDs      []string
	IncludeSeasons  bool
	IncludeEpisodes bool
	VerifyImages    bool
	Checks          []string
	Concurrency     int
}

// Run fetches and checks the selected libraries. Progress is optional and is
// called serially. Operational image failures produce an incomplete report;
// authentication, rate-limit, metadata-fetch and context failures return an error.
func Run(ctx context.Context, client *jellyfin.Client, opts Options, progress func(string)) (audit.Report, error) {
	var r audit.Report
	if err := validateChecks(opts.Checks); err != nil {
		return r, err
	}
	if opts.Concurrency <= 0 {
		return r, fmt.Errorf("concurrency must be positive")
	}
	server, err := client.Server(ctx)
	if err != nil {
		return r, err
	}
	libraries, err := client.Libraries(ctx)
	if err != nil {
		return r, err
	}
	selected, err := selectLibraries(libraries, opts.LibraryIDs)
	if err != nil {
		return r, err
	}
	types := []string{"Movie", "Series"}
	if opts.IncludeSeasons {
		types = append(types, "Season")
	}
	if opts.IncludeEpisodes {
		types = append(types, "Episode")
	}
	r = audit.Report{Server: server, ServerURL: client.BaseURL(), VerifyImages: opts.VerifyImages, Checks: slices.Clone(opts.Checks)}
	seen := make(map[string]bool)
	for _, lib := range selected {
		if progress != nil {
			progress(fmt.Sprintf("Scanning library %s...", lib.ID))
		}
		items, err := client.Items(ctx, lib.ID, types, opts.VerifyImages)
		if err != nil {
			return r, err
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
			r.Results = append(r.Results, audit.Result{Library: lib, Item: item, URL: client.ItemURL(item.ID, server.ID)})
		}
	}
	r.Results, err = checkResults(ctx, client, r.Results, opts.Checks, opts.VerifyImages, opts.Concurrency, progress)
	if err != nil {
		return r, err
	}
	r.Complete = true
	for _, result := range r.Results {
		if len(result.Errors) > 0 {
			r.Complete = false
		}
	}
	r.GeneratedAt = time.Now().UTC()
	audit.Sort(&r)
	return r, nil
}

// CheckResults rechecks existing item snapshots without fetching metadata or
// changing their order, library, item, or URL. Findings and operational errors
// are replaced on a cloned result slice. Empty checks enable no checks.
func CheckResults(ctx context.Context, client *jellyfin.Client, results []audit.Result, checks []string, verifyImages bool, concurrency int) ([]audit.Result, error) {
	return checkResults(ctx, client, results, checks, verifyImages, concurrency, nil)
}

func checkResults(ctx context.Context, client *jellyfin.Client, results []audit.Result, checks []string, verifyImages bool, concurrency int, progress func(string)) ([]audit.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateChecks(checks); err != nil {
		return nil, err
	}
	if verifyImages && concurrency <= 0 {
		return nil, fmt.Errorf("concurrency must be positive")
	}
	checked := slices.Clone(results)
	for i := range checked {
		checked[i].Findings = audit.Check(checked[i].Item, checks)
		checked[i].Errors = nil
	}
	if verifyImages {
		if err := verify(ctx, client, checked, checks, concurrency, progress); err != nil {
			return checked, err
		}
	}
	return checked, ctx.Err()
}

func validateChecks(checks []string) error {
	for _, check := range checks {
		if !slices.Contains(audit.DefaultChecks(), check) {
			return fmt.Errorf("unknown check %q", check)
		}
	}
	return nil
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

func verify(ctx context.Context, client *jellyfin.Client, results []audit.Result, checks []string, concurrency int, progress func(string)) error {
	var jobs []imageJob
	for i, result := range results {
		for _, ref := range audit.ImageRefs(result.Item, checks) {
			jobs = append(jobs, imageJob{i, ref})
		}
	}
	if progress != nil {
		progress(fmt.Sprintf("Verifying %d images...", len(jobs)))
	}
	if len(jobs) == 0 {
		return ctx.Err()
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
				fatal := jellyfin.IsAuthError(err) || jellyfin.IsRateLimitError(err)
				if fatal {
					cancel()
				}
				outcomes <- imageOutcome{job, problem, err}
				if fatal {
					return
				}
			}
		})
	}
	go func() { wg.Wait(); close(outcomes) }()
	var fatal error
	for outcome := range outcomes {
		if jellyfin.IsAuthError(outcome.err) || jellyfin.IsRateLimitError(outcome.err) {
			if fatal == nil {
				fatal = outcome.err
				cancel()
			}
			continue
		}
		result := &results[outcome.job.result]
		if outcome.err != nil {
			result.Errors = append(result.Errors, outcome.err.Error())
			if progress != nil {
				progress(fmt.Sprint("Image verification: ", outcome.err))
			}
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
