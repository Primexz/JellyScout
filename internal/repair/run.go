package repair

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/Primexz/jellyscout/internal/audit"
	"github.com/Primexz/jellyscout/internal/jellyfin"
)

// Run submits refreshes sequentially and observes each submission concurrently.
// Cancellation stops local dispatch and observation only; already submitted
// provider work may continue on the server. Event callbacks are serialized.
func (e *Engine) Run(ctx context.Context, plan Plan, emit func(Event)) error {
	if emit == nil {
		emit = func(Event) {}
	}
	var eventMu sync.Mutex
	send := func(event Event) {
		eventMu.Lock()
		defer eventMu.Unlock()
		emit(event)
	}
	for _, event := range plan.Skipped {
		send(event)
	}
	// Poll failures may stop future dispatch while other submitted actions are
	// still being observed. A separate dispatch context interrupts preflight.
	dispatchCtx, cancelDispatch := context.WithCancel(ctx)
	defer cancelDispatch()
	var stateMu sync.Mutex
	var stopErr error
	var runErrors []error
	stop := func(err error) {
		stateMu.Lock()
		defer stateMu.Unlock()
		if stopErr == nil {
			stopErr = err
			cancelDispatch()
		}
	}
	stopped := func() bool {
		stateMu.Lock()
		defer stateMu.Unlock()
		return stopErr != nil
	}
	record := func(err error) {
		if err != nil {
			stateMu.Lock()
			defer stateMu.Unlock()
			runErrors = append(runErrors, err)
		}
	}
	var polls sync.WaitGroup
	for _, confirmed := range plan.Actions {
		selected := affected(confirmed, plan.Observed)
		id := confirmed.Result.Item.ID
		if ctx.Err() != nil || stopped() {
			message := "Dispatch stopped after authentication or rate-limit failure; review before retrying."
			if ctx.Err() != nil {
				message = "Local repair cancelled; server refreshes already submitted may continue."
			}
			send(Event{ID: id, State: "skipped", Message: message, Results: selected})
			continue
		}
		action, observed, reason, err := e.revalidate(dispatchCtx, confirmed, selected)
		if err != nil {
			if stopDispatch(err) {
				stop(err)
			}
			record(err)
			send(Event{ID: id, State: "skipped", Message: "Pre-dispatch review failed: " + err.Error(), Results: observed})
			continue
		}
		if reason != "" {
			send(Event{ID: id, State: "skipped", Message: reason, Results: observed})
			continue
		}
		if ctx.Err() != nil || stopped() {
			send(Event{ID: id, State: "skipped", Message: "Local dispatch stopped before submission.", Results: observed})
			continue
		}
		err = e.client.Refresh(dispatchCtx, id, action.Metadata, action.Images)
		if err != nil {
			if _, definite := errors.AsType[*jellyfin.StatusError](err); definite {
				if stopDispatch(err) {
					stop(err)
				}
				record(err)
				send(Event{ID: id, State: "failed", Message: err.Error(), Results: observed})
				continue
			}
			// A transport error can happen after the server accepted the POST.
			// Observe once submitted, never repeat the POST automatically.
			send(Event{ID: id, State: "unknown", Message: "Refresh submission outcome is unknown; checking without retrying: " + err.Error(), Results: observed})
		} else {
			send(Event{ID: id, State: "queued", Message: "Provider refresh queued; success requires observed audit changes.", Results: observed})
		}
		postErr := err
		polls.Go(func() {
			record(e.poll(ctx, action, observed, postErr, send, stop))
		})
	}
	polls.Wait()
	if ctx.Err() != nil {
		record(ctx.Err())
	}
	return errors.Join(runErrors...)
}

func stopDispatch(err error) bool { return jellyfin.IsAuthError(err) || jellyfin.IsRateLimitError(err) }

func (e *Engine) revalidate(ctx context.Context, confirmed Action, selected []audit.Result) (Action, []audit.Result, string, error) {
	current, err := e.observe(ctx, selected)
	if err != nil {
		return confirmed, current, "", err
	}
	index := slices.IndexFunc(current, func(result audit.Result) bool { return result.Item.ID == confirmed.Result.Item.ID })
	action := confirmed
	action.Result = current[index]
	if action.Result.Item.Type != confirmed.Result.Item.Type {
		return action, current, "Item type changed; a new review is required.", nil
	}
	if recursive(action.Result.Item) {
		action.Descendants, err = e.client.Descendants(ctx, action.Result.Item.ID)
		if err != nil {
			return action, unavailable(current, err), "", err
		}
		// New descendants were not part of the confirmed blast radius. Changing
		// an existing child's type can also broaden recursive effects.
		known := make(map[string]string, len(confirmed.Descendants))
		for _, item := range confirmed.Descendants {
			known[item.ID] = item.Type
		}
		for _, item := range action.Descendants {
			if kind, ok := known[item.ID]; !ok || kind != item.Type {
				return action, current, "Descendant scope expanded or changed; a new review is required.", nil
			}
		}
	}
	if hasErrors(current) {
		return action, current, "Audit verification failed; recheck before refreshing.", nil
	}
	metadata, images := categories(current)
	safeMetadata, safeImages := safeCategories(action.Result.Item, action.Descendants)
	// Revalidation may only narrow the modes explicitly confirmed in Plan.
	action.Metadata = confirmed.Metadata && metadata && safeMetadata
	action.Images = confirmed.Images && images && safeImages
	if !action.Metadata && !action.Images {
		return action, current, "No confirmed category remains eligible: findings changed or lock settings prevent refresh.", nil
	}
	return action, current, "", nil
}

func (e *Engine) poll(ctx context.Context, action Action, initial []audit.Result, postErr error, send func(Event), stop func(error)) error {
	ctx, cancel := context.WithTimeout(ctx, e.opts.Timeout)
	defer cancel()
	id := action.Result.Item.ID
	latest := initial
	var lastReadErr error
	send(Event{ID: id, State: "checking", Message: "Waiting for observed audit changes; cancelling stops local work only.", Results: latest})
	ticker := time.NewTicker(e.opts.PollInterval)
	defer ticker.Stop()
	finish := func() error {
		state, message := outcome(initial, latest)
		var err error
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			state, message = "unknown", "Local observation cancelled; server refresh may still continue."
			err = ctx.Err()
		case lastReadErr != nil:
			state, message = "failed", "Could not verify the refresh outcome: "+lastReadErr.Error()
			err = lastReadErr
		case postErr != nil && state == "unresolved":
			state, message = "unknown", "Submission outcome remains unknown; no audit improvement was observed. Recheck before another refresh."
			err = postErr
		}
		send(Event{ID: id, State: state, Message: message, Results: latest})
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return finish()
		case <-ticker.C:
			observed, err := e.observe(ctx, initial)
			if ctx.Err() != nil {
				return finish()
			}
			latest = observed
			lastReadErr = err
			if err == nil && hasErrors(latest) {
				lastReadErr = errors.New("audit image verification failed")
			}
			if stopDispatch(err) {
				stop(err)
				send(Event{ID: id, State: "failed", Message: "Observation stopped: " + err.Error(), Results: latest})
				return err
			}
			if lastReadErr == nil {
				if state, message := outcome(initial, latest); state == "resolved" {
					send(Event{ID: id, State: state, Message: message, Results: latest})
					return nil
				}
			}
			send(Event{ID: id, State: "checking", Message: "Refresh is queued; findings have not all disappeared from the audit.", Results: latest})
		}
	}
}

func outcome(initial, current []audit.Result) (state, message string) {
	if !hasErrors(current) && !slices.ContainsFunc(current, func(result audit.Result) bool { return len(result.Findings) != 0 }) {
		return "resolved", "Selected items' findings disappeared in a fresh audit."
	}
	// Count findings per check. A changed error message (for example a missing
	// image becoming an undecodable image) is not evidence of improvement.
	// Multiple backdrop failures can still show partial progress as they fall.
	type findingKey struct{ id, check string }
	remaining := make(map[findingKey]int)
	for _, result := range current {
		for _, finding := range result.Findings {
			remaining[findingKey{result.Item.ID, finding.Check}]++
		}
	}
	for _, result := range initial {
		original := make(map[findingKey]int)
		for _, finding := range result.Findings {
			original[findingKey{result.Item.ID, finding.Check}]++
		}
		for key, count := range original {
			if remaining[key] < count {
				return "partially resolved", "Some findings disappeared; other findings or verification errors remain."
			}
		}
	}
	return "unresolved", "No audit findings disappeared before the observation timeout."
}
