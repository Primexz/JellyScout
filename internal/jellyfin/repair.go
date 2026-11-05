package jellyfin

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ItemsByIDs retrieves fresh audit fields and lock settings in bounded batches.
// It includes virtual items and all requested types. Missing, repeated, or
// unsolicited IDs fail the read rather than making absence look like success.
// Results follow the order of the unique requested IDs.
func (c *Client) ItemsByIDs(ctx context.Context, ids []string, allImages bool) ([]Item, error) {
	unique := make([]string, 0, len(ids))
	requested := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !validID(id) {
			return nil, errors.New("invalid item ID")
		}
		if !requested[id] {
			requested[id] = true
			unique = append(unique, id)
		}
	}
	byID := make(map[string]Item, len(unique))
	// At the maximum valid ID length, 50 IDs still fit common URL limits.
	const batchSize = 50
	for start := 0; start < len(unique); start += batchSize {
		batch := unique[start:min(start+batchSize, len(unique))]
		q := repairQuery()
		q.Set("Ids", strings.Join(batch, ","))
		if !allImages {
			q.Set("ImageTypeLimit", "1")
		}
		items, err := c.queryItems(ctx, q)
		if err != nil {
			return nil, err
		}
		allowed := make(map[string]bool, len(batch))
		for _, id := range batch {
			allowed[id] = true
		}
		for _, item := range items {
			if !allowed[item.ID] {
				return nil, errors.New("Items: targeted response contains an unrequested item")
			}
			byID[item.ID] = item
		}
		if len(items) != len(batch) {
			return nil, errors.New("Items: targeted response is missing requested items")
		}
	}
	items := make([]Item, 0, len(unique))
	for _, id := range unique {
		items = append(items, byID[id])
	}
	return items, nil
}

// Descendants retrieves the entire recursive refresh footprint. In particular,
// it deliberately has no IncludeItemTypes, IsMissing, or location filter: a
// series/season refresh may affect virtual and otherwise unaudited children.
func (c *Client) Descendants(ctx context.Context, parentID string) ([]Item, error) {
	if !validID(parentID) {
		return nil, errors.New("invalid parent ID")
	}
	q := repairQuery()
	q.Set("ParentId", parentID)
	q.Set("Recursive", "true")
	items, err := c.queryItems(ctx, q)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.ID == parentID {
			return nil, errors.New("Items: descendants response contains its own parent")
		}
	}
	return items, nil
}

func repairQuery() url.Values {
	return url.Values{
		"Fields":       {"Overview,Genres,ProviderIds,Path,ParentId,Settings"},
		"EnableImages": {"true"}, "EnableUserData": {"false"},
		"EnableTotalRecordCount": {"true"}, "CollapseBoxSetItems": {"false"},
		"SortBy": {"SortName"}, "SortOrder": {"Ascending"},
		"Limit": {strconv.Itoa(pageSize)},
	}
}

// IsRateLimitError reports HTTP 429 without exposing response content.
func IsRateLimitError(err error) bool {
	e, ok := errors.AsType[*StatusError](err)
	return ok && e.StatusCode == http.StatusTooManyRequests
}

// Refresh requests provider refreshes with replacement and trickplay flags off.
// Provider behavior may still change existing data. Only HTTP 204 means queued;
// it never means repaired.
// There is exactly one POST attempt. A non-StatusError may have reached the
// server and must be checked by reading the item, never retried automatically.
func (c *Client) Refresh(ctx context.Context, itemID string, metadata, images bool) error {
	if !validID(itemID) {
		return errors.New("invalid item ID")
	}
	if !metadata && !images {
		return errors.New("at least one refresh category is required")
	}
	mode := func(enabled bool) string {
		if enabled {
			return "FullRefresh"
		}
		return "None"
	}
	q := url.Values{
		"metadataRefreshMode": {mode(metadata)}, "imageRefreshMode": {mode(images)},
		"replaceAllMetadata": {"false"}, "replaceAllImages": {"false"},
		"regenerateTrickplay": {"false"},
	}
	op := "Items/" + itemID + "/Refresh"
	resp, err := c.request(ctx, http.MethodPost, op, q)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return &StatusError{StatusCode: resp.StatusCode, Operation: c.redact(op)}
	}
	return nil
}
