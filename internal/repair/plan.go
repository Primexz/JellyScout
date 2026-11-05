package repair

import (
	"cmp"
	"context"
	"slices"

	"github.com/Primexz/jellyscout/internal/audit"
	"github.com/Primexz/jellyscout/internal/jellyfin"
)

// Plan rechecks selected IDs and reviews every descendant before proposing a
// recursive refresh. It has no side effects on the Jellyfin server.
func (e *Engine) Plan(ctx context.Context, selected []audit.Result) (Plan, error) {
	var plan Plan
	seen := make(map[string]bool, len(selected))
	unique := make([]audit.Result, 0, len(selected))
	for _, result := range selected {
		if !seen[result.Item.ID] {
			seen[result.Item.ID] = true
			unique = append(unique, result)
		}
	}
	var err error
	plan.Observed, err = e.observe(ctx, unique)
	if err != nil {
		return plan, err
	}
	for _, result := range plan.Observed {
		action := Action{Result: result}
		action.Metadata, action.Images = categories([]audit.Result{result})
		message := ""
		switch {
		case !supported(result.Item):
			message = "Item type is not supported for repair."
		case len(result.Errors) > 0:
			message = "Audit verification failed; recheck before refreshing."
		case !action.Metadata && !action.Images:
			message = "No current findings need a provider refresh."
		}
		if message == "" {
			if recursive(result.Item) {
				action.Descendants, err = e.client.Descendants(ctx, result.Item.ID)
				if err != nil {
					return plan, err
				}
			}
			metadata, images := safeCategories(result.Item, action.Descendants)
			action.Metadata = action.Metadata && metadata
			action.Images = action.Images && images
			if !action.Metadata && !action.Images {
				message = "Item or descendants have locked fields or unknown lock settings."
			}
		}
		if message != "" {
			plan.Skipped = append(plan.Skipped, Event{ID: result.Item.ID, State: "skipped", Message: message, Results: []audit.Result{result}})
			continue
		}
		plan.Actions = append(plan.Actions, action)
	}
	// Put selected ancestors first regardless of the selection's display order.
	slices.SortStableFunc(plan.Actions, func(a, b Action) int {
		return cmp.Compare(depth(a), depth(b))
	})
	var actions []Action
	for _, action := range plan.Actions {
		for _, ancestor := range actions {
			if slices.ContainsFunc(ancestor.Descendants, func(item jellyfin.Item) bool { return item.ID == action.Result.Item.ID }) {
				action.Metadata = action.Metadata && !ancestor.Metadata
				action.Images = action.Images && !ancestor.Images
			}
		}
		if !action.Metadata && !action.Images {
			plan.Skipped = append(plan.Skipped, Event{ID: action.Result.Item.ID, State: "skipped", Message: "Confirmed categories are covered by an ancestor refresh.", Results: []audit.Result{action.Result}})
			continue
		}
		actions = append(actions, action)
	}
	plan.Actions = actions
	return plan, ctx.Err()
}

func depth(action Action) int {
	switch action.Result.Item.Type {
	case "Series":
		return 0
	case "Season":
		return 1
	default:
		return 2
	}
}
