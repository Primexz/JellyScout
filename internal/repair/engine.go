// Package repair plans conservative provider refreshes and observes their audit
// outcomes. A queued refresh is never evidence that a finding was repaired.
package repair

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/Primexz/jellyscout/internal/audit"
	"github.com/Primexz/jellyscout/internal/jellyfin"
	"github.com/Primexz/jellyscout/internal/scan"
)

type Options struct {
	Checks                []string
	VerifyImages          bool
	Concurrency           int
	Timeout, PollInterval time.Duration
}

type Engine struct {
	client       *jellyfin.Client
	opts         Options
	observations chan struct{}
}

type Plan struct {
	Actions  []Action
	Skipped  []Event
	Observed []audit.Result
}

type Action struct {
	Result           audit.Result
	Metadata, Images bool
	// Descendants is the complete, unfiltered recursive refresh footprint that
	// must be shown at confirmation, including items outside the audit scope.
	Descendants []jellyfin.Item
}

type Event struct {
	ID, State, Message string
	// Results contains fresh audits for the action and selected descendants.
	// Callers should merge by ID; an empty slice does not erase prior audits.
	Results []audit.Result
}

func New(client *jellyfin.Client, opts Options) *Engine {
	opts.Checks = slices.Clone(opts.Checks)
	if opts.Checks == nil {
		opts.Checks = audit.DefaultChecks()
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 2 * time.Minute
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 2 * time.Second
	}
	return &Engine{client: client, opts: opts, observations: make(chan struct{}, 1)}
}

func (e *Engine) observe(ctx context.Context, selected []audit.Result) ([]audit.Result, error) {
	// Image verification has its own bounded workers. Serializing complete
	// observations keeps that bound global even with many pending refreshes.
	select {
	case e.observations <- struct{}{}:
		defer func() { <-e.observations }()
	case <-ctx.Done():
		return unavailable(selected, ctx.Err()), ctx.Err()
	}
	ids := make([]string, 0, len(selected))
	for _, result := range selected {
		ids = append(ids, result.Item.ID)
	}
	items, err := e.client.ItemsByIDs(ctx, ids, e.opts.VerifyImages)
	if err != nil {
		return unavailable(selected, err), err
	}
	byID := make(map[string]jellyfin.Item, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	results := slices.Clone(selected)
	for i := range results {
		item, ok := byID[results[i].Item.ID]
		if !ok {
			err := errors.New("requested item is unavailable; audit disappearance is unverified")
			return unavailable(selected, err), err
		}
		results[i].Item = item
	}
	return scan.CheckResults(ctx, e.client, results, e.opts.Checks, e.opts.VerifyImages, e.opts.Concurrency)
}

func unavailable(results []audit.Result, err error) []audit.Result {
	results = slices.Clone(results)
	for i := range results {
		results[i].Errors = append(slices.Clone(results[i].Errors), err.Error())
	}
	return results
}

func categories(results []audit.Result) (metadata, images bool) {
	for _, result := range results {
		for _, finding := range result.Findings {
			switch finding.Check {
			case "primary-image", "backdrop":
				images = true
			case "title", "overview", "year", "genres", "provider-id", "numbering":
				metadata = true
			}
		}
	}
	return metadata, images
}

func recursive(item jellyfin.Item) bool { return item.Type == "Series" || item.Type == "Season" }

func supported(item jellyfin.Item) bool {
	return recursive(item) || item.Type == "Movie" || item.Type == "Episode"
}

func hasErrors(results []audit.Result) bool {
	return slices.ContainsFunc(results, func(result audit.Result) bool { return len(result.Errors) != 0 })
}

// Refresh categories cannot target a single metadata field. Any metadata field
// lock therefore blocks the metadata category, while still permitting artwork.
// Unknown/missing settings and unknown field names block both categories.
func allowed(item jellyfin.Item) (metadata, images bool) {
	if item.LockData == nil || *item.LockData || item.LockedFields == nil {
		return false, false
	}
	metadata, images = true, true
	for _, field := range item.LockedFields {
		switch field {
		case "Cast", "Genres", "ProductionLocations", "Studios", "Tags", "Name", "Overview", "Runtime", "OfficialRating":
			metadata = false
		default:
			return false, false
		}
	}
	return metadata, images
}

func safeCategories(item jellyfin.Item, descendants []jellyfin.Item) (metadata, images bool) {
	metadata, images = allowed(item)
	for _, child := range descendants {
		m, i := allowed(child)
		metadata = metadata && m
		images = images && i
	}
	return metadata, images
}

func affected(action Action, selected []audit.Result) []audit.Result {
	ids := map[string]bool{action.Result.Item.ID: true}
	for _, item := range action.Descendants {
		ids[item.ID] = true
	}
	var results []audit.Result
	for _, result := range selected {
		if ids[result.Item.ID] {
			results = append(results, result)
		}
	}
	if !slices.ContainsFunc(results, func(result audit.Result) bool { return result.Item.ID == action.Result.Item.ID }) {
		results = append(results, action.Result)
	}
	return results
}
