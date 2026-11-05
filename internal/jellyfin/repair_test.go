package jellyfin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestItemsByIDsBatchesFreshSettingsAndOrder(t *testing.T) {
	var requests int
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		q := r.URL.Query()
		if r.URL.Path != "/jellyfin/Items" || q.Has("ParentId") || q.Has("IncludeItemTypes") || q.Has("IsMissing") {
			t.Errorf("unwanted scope restriction: %s", r.URL)
		}
		if !strings.Contains(q.Get("Fields"), "Settings") || q.Has("ImageTypeLimit") {
			t.Errorf("missing fresh settings or all artwork: %v", q)
		}
		ids := strings.Split(q.Get("Ids"), ",")
		if len(ids) > 50 {
			t.Errorf("unbounded targeted read: %d IDs", len(ids))
		}
		slices.Reverse(ids)
		items := make([]Item, 0, len(ids))
		for _, id := range ids {
			items = append(items, Item{ID: id, LockData: new(false), LockedFields: []string{}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Items": items, "TotalRecordCount": len(items)})
	})
	var ids []string
	for i := range 103 {
		ids = append(ids, "item-"+strconv.Itoa(i))
	}
	items, err := c.ItemsByIDs(t.Context(), append(slices.Clone(ids), ids[0]), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != len(ids) || requests != 3 {
		t.Fatalf("items=%d requests=%d", len(items), requests)
	}
	for i, item := range items {
		if item.ID != ids[i] || item.LockData == nil || *item.LockData || item.LockedFields == nil {
			t.Fatalf("item[%d]=%+v", i, item)
		}
	}
}

func TestTargetedReadRejectsMissingUnexpectedAndUnknownSettings(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		wantError  bool
	}{
		{"missing", `{"Items":[],"TotalRecordCount":0}`, true},
		{"unexpected", `{"Items":[{"Id":"other"}],"TotalRecordCount":1}`, true},
		{"unknown locks retained", `{"Items":[{"Id":"a"}],"TotalRecordCount":1}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, tt.body) })
			items, err := c.ItemsByIDs(t.Context(), []string{"a"}, false)
			if (err != nil) != tt.wantError {
				t.Fatalf("items=%v err=%v", items, err)
			}
			if err == nil && (items[0].LockData != nil || items[0].LockedFields != nil) {
				t.Fatal("missing settings must not decode as unlocked")
			}
		})
	}
}

func TestDescendantsUnfilteredAndPaginated(t *testing.T) {
	var starts []string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		for _, filter := range []string{"IsMissing", "IncludeItemTypes", "LocationTypes", "ImageTypeLimit", "Ids"} {
			if q.Has(filter) {
				t.Errorf("descendants must not filter %s", filter)
			}
		}
		if q.Get("ParentId") != "series" || q.Get("Recursive") != "true" || !strings.Contains(q.Get("Fields"), "Settings") {
			t.Errorf("query=%v", q)
		}
		starts = append(starts, q.Get("StartIndex"))
		item := Item{ID: "season", Type: "Season", LockData: new(false), LockedFields: []string{}}
		if q.Get("StartIndex") == "1" {
			item = Item{ID: "virtual", Type: "Episode", LocationType: "Virtual", LockData: new(true), LockedFields: []string{}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Items": []Item{item}, "TotalRecordCount": 2})
	})
	items, err := c.Descendants(t.Context(), "series")
	if err != nil || len(items) != 2 || !slices.Equal(starts, []string{"0", "1"}) || items[1].LocationType != "Virtual" {
		t.Fatalf("items=%v starts=%v err=%v", items, starts, err)
	}
}

func TestRepairIDsValidatedBeforeRequests(t *testing.T) {
	var hits atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })
	for _, id := range []string{"", "../secret", "a/b", "a?api_key=secret", strings.Repeat("x", 129)} {
		if _, err := c.ItemsByIDs(t.Context(), []string{"valid", id}, true); err == nil {
			t.Errorf("accepted targeted ID %q", id)
		}
		if _, err := c.Descendants(t.Context(), id); err == nil {
			t.Errorf("accepted parent ID %q", id)
		}
		if err := c.Refresh(t.Context(), id, true, true); err == nil {
			t.Errorf("accepted refresh ID %q", id)
		}
	}
	if err := c.Refresh(t.Context(), "valid", false, false); err == nil {
		t.Fatal("empty refresh accepted")
	}
	if hits.Load() != 0 {
		t.Fatalf("invalid input caused %d requests", hits.Load())
	}
}

func TestRefreshModesQueueSemanticsAndNoRetry(t *testing.T) {
	for _, tt := range []struct {
		metadata, images bool
	}{{true, false}, {false, true}, {true, true}} {
		for _, status := range []int{204, 200, 202, 401, 403, 429, 500} {
			t.Run(fmt.Sprintf("metadata=%v/images=%v/status=%d", tt.metadata, tt.images, status), func(t *testing.T) {
				var hits int
				c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
					hits++
					if r.Method != "POST" || r.URL.Path != "/jellyfin/Items/movie/Refresh" || r.URL.RawQuery == "" {
						t.Errorf("request=%s %s", r.Method, r.URL)
					}
					if r.Header.Get("Authorization") != `MediaBrowser Token="test-api-secret"` || r.URL.Query().Has("api_key") {
						t.Error("unsafe authentication")
					}
					q := r.URL.Query()
					for key, enabled := range map[string]bool{"metadataRefreshMode": tt.metadata, "imageRefreshMode": tt.images} {
						want := "None"
						if enabled {
							want = "FullRefresh"
						}
						if q.Get(key) != want {
							t.Errorf("%s=%q want %q", key, q.Get(key), want)
						}
					}
					for _, key := range []string{"replaceAllMetadata", "replaceAllImages", "regenerateTrickplay"} {
						if q.Get(key) != "false" {
							t.Errorf("%s=%q", key, q.Get(key))
						}
					}
					w.WriteHeader(status)
					if status != 204 {
						_, _ = fmt.Fprint(w, "test-api-secret")
					}
				})
				err := c.Refresh(t.Context(), "movie", tt.metadata, tt.images)
				if (err == nil) != (status == 204) || hits != 1 {
					t.Fatalf("err=%v hits=%d", err, hits)
				}
				if err != nil && strings.Contains(err.Error(), "test-api-secret") {
					t.Fatalf("unsafe error: %v", err)
				}
				if IsRateLimitError(err) != (status == 429) {
					t.Fatalf("rate limit classification: %v", err)
				}
			})
		}
	}
}

func TestRefreshDoesNotFollowRedirects(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits.Add(1) }))
	t.Cleanup(target.Close)
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	if err := c.Refresh(t.Context(), "a", true, false); err == nil || targetHits.Load() != 0 {
		t.Fatalf("err=%v redirected hits=%d", err, targetHits.Load())
	}
}
