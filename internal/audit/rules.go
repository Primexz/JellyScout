package audit

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/Primexz/jellyscout/internal/jellyfin"
)

// DefaultChecks returns the supported checks in their canonical order.
func DefaultChecks() []string {
	return []string{
		"primary-image", "backdrop", "title", "overview",
		"year", "genres", "provider-id", "numbering",
	}
}

// ParseChecks validates a comma-separated list and returns unique checks in
// canonical order. An empty list or an empty entry is an error.
func ParseChecks(value string) ([]string, error) {
	enabled := make(map[string]bool)
	canonical := DefaultChecks()
	for token := range strings.SplitSeq(value, ",") {
		check := strings.TrimSpace(token)
		if check == "" {
			return nil, fmt.Errorf("check names must not be empty")
		}
		if !slices.Contains(canonical, check) {
			return nil, fmt.Errorf("unknown check %q", check)
		}
		enabled[check] = true
	}
	var checks []string
	for _, check := range canonical {
		if enabled[check] {
			checks = append(checks, check)
		}
	}
	return checks, nil
}

// Check reports missing metadata for enabled checks applicable to the item type.
// A nil or empty checks slice enables no checks.
func Check(item jellyfin.Item, checks []string) []Finding {
	fullMetadata := item.Type == "Movie" || item.Type == "Series"
	if !fullMetadata && item.Type != "Season" && item.Type != "Episode" {
		return nil
	}

	var findings []Finding
	for _, check := range DefaultChecks() {
		if !slices.Contains(checks, check) {
			continue
		}
		var message string
		switch check {
		case "primary-image":
			if !nonblank(item.ImageTags["Primary"]) {
				message = "Missing own primary image."
			}
		case "backdrop":
			if fullMetadata && !slices.ContainsFunc(item.BackdropImageTags, nonblank) {
				message = "Missing own backdrop image."
			}
		case "title":
			if !nonblank(item.Name) {
				message = "Missing title."
			}
		case "overview":
			if (fullMetadata || item.Type == "Episode") && !nonblank(item.Overview) {
				message = "Missing overview."
			}
		case "year":
			if fullMetadata && (item.ProductionYear == nil || *item.ProductionYear <= 0) {
				message = "Missing positive production year."
			}
		case "genres":
			if fullMetadata && !slices.ContainsFunc(item.Genres, nonblank) {
				message = "Missing genre."
			}
		case "provider-id":
			if fullMetadata && !hasProviderID(item.ProviderIDs) {
				message = "Missing IMDb, TMDb, or TVDb provider ID."
			}
		case "numbering":
			switch item.Type {
			case "Season":
				if !nonnegative(item.IndexNumber) {
					message = "Missing nonnegative season number."
				}
			case "Episode":
				if !nonnegative(item.IndexNumber) || !nonnegative(item.ParentIndexNumber) {
					message = "Missing nonnegative episode or season number."
				}
			}
		}
		if message != "" {
			findings = append(findings, Finding{Check: check, Message: message})
		}
	}
	return findings
}

// ImageRefs returns only the item's own images for applicable, enabled checks.
// Backdrop indices refer to the original slice, including any blank entries.
func ImageRefs(item jellyfin.Item, checks []string) []jellyfin.ImageRef {
	fullMetadata := item.Type == "Movie" || item.Type == "Series"
	if !fullMetadata && item.Type != "Season" && item.Type != "Episode" {
		return nil
	}
	var refs []jellyfin.ImageRef
	if tag := item.ImageTags["Primary"]; slices.Contains(checks, "primary-image") && nonblank(tag) {
		refs = append(refs, jellyfin.ImageRef{ItemID: item.ID, Type: "Primary", Tag: tag})
	}
	if fullMetadata && slices.Contains(checks, "backdrop") {
		for index, tag := range item.BackdropImageTags {
			if nonblank(tag) {
				refs = append(refs, jellyfin.ImageRef{ItemID: item.ID, Type: "Backdrop", Index: index, Tag: tag})
			}
		}
	}
	return refs
}

// Sort orders results by library name/ID, item type, title, and item ID. Findings
// use canonical check order with message tie-breakers; errors retain their text
// and duplicates and are sorted lexically. Equal entries retain their order.
func Sort(report *Report) {
	slices.SortStableFunc(report.Results, func(a, b Result) int {
		return cmp.Or(
			cmp.Compare(a.Library.Name, b.Library.Name),
			cmp.Compare(a.Library.ID, b.Library.ID),
			cmp.Compare(a.Item.Type, b.Item.Type),
			cmp.Compare(a.Item.Name, b.Item.Name),
			cmp.Compare(a.Item.ID, b.Item.ID),
		)
	})
	canonical := DefaultChecks()
	for i := range report.Results {
		result := &report.Results[i]
		slices.SortStableFunc(result.Findings, func(a, b Finding) int {
			return cmp.Or(
				cmp.Compare(checkOrder(canonical, a.Check), checkOrder(canonical, b.Check)),
				cmp.Compare(a.Check, b.Check),
				cmp.Compare(a.Message, b.Message),
			)
		})
		slices.SortStableFunc(result.Errors, strings.Compare)
	}
}

func checkOrder(canonical []string, check string) int {
	if index := slices.Index(canonical, check); index >= 0 {
		return index
	}
	return len(canonical)
}

func nonblank(value string) bool {
	return strings.TrimSpace(value) != ""
}

func nonnegative(value *int) bool {
	return value != nil && *value >= 0
}

func hasProviderID(ids map[string]string) bool {
	for key, value := range ids {
		if (strings.EqualFold(key, "imdb") || strings.EqualFold(key, "tmdb") || strings.EqualFold(key, "tvdb")) && nonblank(value) {
			return true
		}
	}
	return false
}
