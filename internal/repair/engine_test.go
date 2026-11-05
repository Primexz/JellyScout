package repair

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Primexz/jellyscout/internal/audit"
	"github.com/Primexz/jellyscout/internal/jellyfin"
)

type submission struct {
	id               string
	metadata, images string
}

type fixture struct {
	mu          sync.Mutex
	items       map[string]jellyfin.Item
	descendants map[string][]string
	posts       []submission
	onPost      func(http.ResponseWriter, *http.Request, *fixture) bool // called under mu
	intercept   func(http.ResponseWriter, *http.Request) bool           // called outside mu
}

func newFixture(t *testing.T, items []jellyfin.Item, opts Options) (*fixture, *Engine) {
	t.Helper()
	f := &fixture{items: make(map[string]jellyfin.Item), descendants: make(map[string][]string)}
	for _, item := range items {
		f.items[item.ID] = item
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.intercept != nil && f.intercept(w, r) {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != `MediaBrowser Token="test-secret"` {
			t.Error("authentication missing")
		}
		if r.Method == "POST" {
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/base/Items/"), "/Refresh")
			q := r.URL.Query()
			f.posts = append(f.posts, submission{id, q.Get("metadataRefreshMode"), q.Get("imageRefreshMode")})
			if f.onPost != nil && f.onPost(w, r, f) {
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != "GET" || r.URL.Path != "/base/Items" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(500)
			return
		}
		q := r.URL.Query()
		if !strings.Contains(q.Get("Fields"), "Settings") {
			t.Error("repair must read fresh settings")
		}
		ids := f.descendants[q.Get("ParentId")]
		if q.Has("Ids") {
			ids = strings.Split(q.Get("Ids"), ",")
		} else if q.Has("IsMissing") || q.Has("IncludeItemTypes") || q.Has("ImageTypeLimit") {
			t.Error("recursive review must include every descendant and image")
		}
		items := make([]jellyfin.Item, 0, len(ids))
		for _, id := range ids {
			if item, ok := f.items[id]; ok {
				items = append(items, item)
			}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"Items": items, "TotalRecordCount": len(items)}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	client, err := jellyfin.New(server.URL+"/base", "test-secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	if opts.Timeout == 0 {
		opts.Timeout = 250 * time.Millisecond
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = time.Millisecond
	}
	return f, New(client, opts)
}

func unlocked(id, itemType string) jellyfin.Item {
	return jellyfin.Item{ID: id, Type: itemType, Name: id, LockData: new(false), LockedFields: []string{}}
}

func selected(items ...jellyfin.Item) []audit.Result {
	var results []audit.Result
	for _, item := range items {
		results = append(results, audit.Result{Item: item, Findings: []audit.Finding{{Check: "overview", Message: "Stale finding"}}})
	}
	return results
}

func collectRun(t *testing.T, engine *Engine, plan Plan) ([]Event, error) {
	t.Helper()
	var events []Event
	err := engine.Run(t.Context(), plan, func(event Event) { events = append(events, event) })
	return events, err
}

func finalEvent(t *testing.T, events []Event, id string) Event {
	t.Helper()
	for _, event := range slices.Backward(events) {
		if event.ID == id {
			return event
		}
	}
	t.Fatalf("no event for %s: %+v", id, events)
	return Event{}
}

func mustPlan(t *testing.T, engine *Engine, results []audit.Result) Plan {
	t.Helper()
	plan, err := engine.Plan(t.Context(), results)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestPlanUsesFreshAuditAndConservativeLocks(t *testing.T) {
	for _, tt := range []struct {
		name             string
		lock             *bool
		fields           []string
		metadata, images bool
	}{
		{"unlocked", new(false), []string{}, true, true},
		{"full lock", new(true), []string{}, false, false},
		{"missing full lock setting", nil, []string{}, false, false},
		{"missing field lock setting", new(false), nil, false, false},
		{"known field lock allows artwork", new(false), []string{"Overview"}, false, true},
		{"unknown field lock", new(false), []string{"FutureField"}, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			item := unlocked("a", "Movie")
			item.LockData, item.LockedFields = tt.lock, tt.fields
			f, engine := newFixture(t, []jellyfin.Item{item}, Options{Checks: []string{"overview", "primary-image"}})
			stale := item
			stale.Overview = "Stale populated data"
			plan := mustPlan(t, engine, selected(stale))
			if len(plan.Observed) != 1 || plan.Observed[0].Item.Overview != "" || len(plan.Observed[0].Findings) != 2 {
				t.Fatalf("stale audit was not replaced: %+v", plan)
			}
			if tt.metadata || tt.images {
				if len(plan.Actions) != 1 || plan.Actions[0].Metadata != tt.metadata || plan.Actions[0].Images != tt.images {
					t.Fatalf("plan=%+v", plan)
				}
			} else if len(plan.Actions) != 0 || len(plan.Skipped) != 1 {
				t.Fatalf("unsafe plan=%+v", plan)
			}
			if len(f.posts) != 0 {
				t.Fatal("preflight submitted a refresh")
			}
		})
	}
}

func TestPlanReviewsVirtualOutsideScopeAndDeduplicatesPerCategory(t *testing.T) {
	series := unlocked("series", "Series")
	season := unlocked("season", "Season")
	season.ImageTags = map[string]string{"Primary": "season-image"}
	episode := unlocked("episode", "Episode")
	episode.SeriesID, episode.SeasonID = series.ID, season.ID
	series.ImageTags = map[string]string{"Primary": "series-image"}
	virtual := unlocked("virtual", "Episode")
	virtual.LocationType = "Virtual"
	virtual.LockedFields = []string{"Name"}
	f, engine := newFixture(t, []jellyfin.Item{series, season, episode, virtual}, Options{Checks: []string{"overview", "primary-image"}})
	f.descendants[series.ID] = []string{season.ID, episode.ID}
	f.descendants[season.ID] = []string{episode.ID}
	plan := mustPlan(t, engine, selected(episode, series, season))
	if len(plan.Actions) != 2 || plan.Actions[0].Result.Item.ID != series.ID || !plan.Actions[0].Metadata || plan.Actions[0].Images {
		t.Fatalf("ancestor categories=%+v", plan.Actions)
	}
	if child := plan.Actions[1]; child.Result.Item.ID != episode.ID || child.Metadata || !child.Images {
		t.Fatalf("category-aware dedup lost child artwork: %+v", child)
	}
	if len(plan.Actions[0].Descendants) != 2 || len(plan.Observed) != 3 {
		t.Fatalf("missing reviewed or selected descendants: %+v", plan)
	}
	// This virtual item is deliberately outside the audit selection. Its field
	// lock still prevents a recursive metadata category from being proposed.
	f.descendants[series.ID] = append(f.descendants[series.ID], virtual.ID)
	plan = mustPlan(t, engine, selected(series))
	if len(plan.Actions) != 0 || len(plan.Skipped) != 1 {
		t.Fatalf("ignored outside-scope virtual lock: %+v", plan)
	}
}

func TestQueuedWithoutObservedChangeIsUnresolved(t *testing.T) {
	item := unlocked("a", "Movie")
	f, engine := newFixture(t, []jellyfin.Item{item}, Options{Checks: []string{"overview"}})
	plan := mustPlan(t, engine, selected(item))
	events, err := collectRun(t, engine, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.posts) != 1 || finalEvent(t, events, item.ID).State != "unresolved" {
		t.Fatalf("204 was treated as repaired: posts=%v events=%+v", f.posts, events)
	}
	if !slices.ContainsFunc(events, func(event Event) bool { return event.State == "queued" }) {
		t.Fatal("missing queued event")
	}
}

func TestChangedImageFailureMessageIsNotPartialResolution(t *testing.T) {
	before := []audit.Result{{Item: unlocked("a", "Movie"), Findings: []audit.Finding{{Check: "primary-image", Message: "Missing own primary image."}}}}
	after := []audit.Result{{Item: unlocked("a", "Movie"), Findings: []audit.Finding{{Check: "primary-image", Message: "Primary image response contains invalid image data"}}}}
	if state, _ := outcome(before, after); state != "unresolved" {
		t.Fatalf("a different artwork failure was counted as repaired: %s", state)
	}
}

func TestRunObservesResolutionAndPartialResolution(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%v", partial), func(t *testing.T) {
			item := unlocked("a", "Movie")
			f, engine := newFixture(t, []jellyfin.Item{item}, Options{Checks: []string{"overview", "genres"}})
			f.onPost = func(w http.ResponseWriter, r *http.Request, f *fixture) bool {
				updated := f.items[item.ID]
				updated.Overview = "Provider overview"
				if !partial {
					updated.Genres = []string{"Drama"}
				}
				f.items[item.ID] = updated
				return false
			}
			events, err := collectRun(t, engine, mustPlan(t, engine, selected(item)))
			if err != nil {
				t.Fatal(err)
			}
			want := "resolved"
			if partial {
				want = "partially resolved"
			}
			last := finalEvent(t, events, item.ID)
			if last.State != want || len(last.Results) != 1 || last.Results[0].Item.Overview == "" {
				t.Fatalf("last=%+v", last)
			}
		})
	}
}

func TestRunDoesNotBroadenModesAndRevalidatesLocksAndScope(t *testing.T) {
	for _, change := range []string{"new image finding", "full lock", "unknown locks", "descendant locked", "added descendant"} {
		t.Run(change, func(t *testing.T) {
			series := unlocked("series", "Series")
			series.ImageTags = map[string]string{"Primary": "tag"}
			child := unlocked("child", "Episode")
			f, engine := newFixture(t, []jellyfin.Item{series, child}, Options{Checks: []string{"overview", "primary-image"}})
			f.descendants[series.ID] = []string{child.ID}
			plan := mustPlan(t, engine, selected(series))
			if len(plan.Actions) != 1 || !plan.Actions[0].Metadata || plan.Actions[0].Images {
				t.Fatalf("unexpected plan=%+v", plan)
			}
			f.mu.Lock()
			switch change {
			case "new image finding":
				series.ImageTags = nil
			case "full lock":
				series.LockData = new(true)
			case "unknown locks":
				series.LockedFields = nil
			case "descendant locked":
				child.LockData = new(true)
				f.items[child.ID] = child
			case "added descendant":
				f.items["new"] = unlocked("new", "Episode")
				f.descendants[series.ID] = append(f.descendants[series.ID], "new")
			}
			f.items[series.ID] = series
			f.mu.Unlock()
			events, err := collectRun(t, engine, plan)
			if err != nil {
				t.Fatal(err)
			}
			if change == "new image finding" {
				if len(f.posts) != 1 || f.posts[0].images != "None" || f.posts[0].metadata != "FullRefresh" {
					t.Fatalf("broadened confirmed modes: %v", f.posts)
				}
			} else if len(f.posts) != 0 || finalEvent(t, events, series.ID).State != "skipped" {
				t.Fatalf("unsafe refresh: posts=%v events=%+v", f.posts, events)
			}
		})
	}
}

func TestRunIncludesDeduplicatedDescendantsWhenParentAlreadyHealthy(t *testing.T) {
	series := unlocked("series", "Series")
	episode := unlocked("episode", "Episode")
	f, engine := newFixture(t, []jellyfin.Item{series, episode}, Options{Checks: []string{"overview"}})
	f.descendants[series.ID] = []string{episode.ID}
	plan := mustPlan(t, engine, selected(episode, series))
	if len(plan.Actions) != 1 || plan.Actions[0].Result.Item.ID != series.ID {
		t.Fatalf("plan=%+v", plan)
	}
	series.Overview = "Already repaired elsewhere"
	f.items[series.ID] = series
	f.onPost = func(w http.ResponseWriter, r *http.Request, f *fixture) bool {
		episode.Overview = "Provider fixed child"
		f.items[episode.ID] = episode
		return false
	}
	events, err := collectRun(t, engine, plan)
	if err != nil {
		t.Fatal(err)
	}
	last := finalEvent(t, events, series.ID)
	if len(f.posts) != 1 || last.State != "resolved" || len(last.Results) != 2 {
		t.Fatalf("child was not observed: posts=%v last=%+v", f.posts, last)
	}
	child := last.Results[slices.IndexFunc(last.Results, func(result audit.Result) bool { return result.Item.ID == episode.ID })]
	if child.Item.Overview == "" || len(child.Findings) != 0 {
		t.Fatalf("stale child=%+v", child)
	}
}

func TestRunSubmitsSequentiallyWhilePollingAsynchronously(t *testing.T) {
	a, b := unlocked("a", "Movie"), unlocked("b", "Movie")
	f, engine := newFixture(t, []jellyfin.Item{a, b}, Options{Checks: []string{"overview"}, Timeout: time.Second})
	f.onPost = func(w http.ResponseWriter, r *http.Request, f *fixture) bool {
		// The first action can resolve only once the second has been submitted.
		if len(f.posts) == 2 {
			for _, id := range []string{a.ID, b.ID} {
				item := f.items[id]
				item.Overview = "Provider overview"
				f.items[id] = item
			}
		}
		return false
	}
	events, err := collectRun(t, engine, mustPlan(t, engine, selected(a, b)))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.posts) != 2 || f.posts[0].id != a.ID || f.posts[1].id != b.ID || finalEvent(t, events, a.ID).State != "resolved" || finalEvent(t, events, b.ID).State != "resolved" {
		t.Fatalf("dispatch waited for polling: posts=%v events=%+v", f.posts, events)
	}
}

func TestRunStopsDispatchOnAuthenticationAndRateLimits(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			a, b := unlocked("a", "Movie"), unlocked("b", "Movie")
			f, engine := newFixture(t, []jellyfin.Item{a, b}, Options{Checks: []string{"overview"}})
			f.onPost = func(w http.ResponseWriter, r *http.Request, f *fixture) bool { w.WriteHeader(status); return true }
			events, err := collectRun(t, engine, mustPlan(t, engine, selected(a, b)))
			if err == nil || len(f.posts) != 1 || finalEvent(t, events, a.ID).State != "failed" || finalEvent(t, events, b.ID).State != "skipped" {
				t.Fatalf("posts=%v events=%+v err=%v", f.posts, events, err)
			}
		})
	}
}

func TestAmbiguousPOSTIsObservedWithoutRetry(t *testing.T) {
	for _, improves := range []bool{false, true} {
		t.Run(fmt.Sprintf("improves=%v", improves), func(t *testing.T) {
			item := unlocked("a", "Movie")
			f, engine := newFixture(t, []jellyfin.Item{item}, Options{Checks: []string{"overview"}})
			f.onPost = func(w http.ResponseWriter, r *http.Request, f *fixture) bool {
				if improves {
					item.Overview = "Provider worked despite lost response"
					f.items[item.ID] = item
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return true
				}
				_ = conn.Close()
				return true
			}
			events, err := collectRun(t, engine, mustPlan(t, engine, selected(item)))
			want := "unknown"
			if improves {
				want = "resolved"
			}
			if len(f.posts) != 1 || finalEvent(t, events, item.ID).State != want || (err == nil) != improves {
				t.Fatalf("posts=%v events=%+v err=%v", f.posts, events, err)
			}
			if slices.ContainsFunc(events, func(event Event) bool { return event.State == "queued" }) {
				t.Fatal("ambiguous POST must not be called queued")
			}
		})
	}
}

func TestMissingItemNeverLosesIdentityOrResolves(t *testing.T) {
	item := unlocked("a", "Movie")
	f, engine := newFixture(t, []jellyfin.Item{item}, Options{Checks: []string{"overview"}})
	plan := mustPlan(t, engine, selected(item))
	f.onPost = func(w http.ResponseWriter, r *http.Request, f *fixture) bool { delete(f.items, item.ID); return false }
	events, err := collectRun(t, engine, plan)
	last := finalEvent(t, events, item.ID)
	if err == nil || last.State != "failed" || len(last.Results) != 1 || last.Results[0].Item.ID != item.ID || len(last.Results[0].Errors) == 0 || len(last.Results[0].Findings) == 0 {
		t.Fatalf("missing item became healthy: last=%+v err=%v", last, err)
	}
	plan, err = engine.Plan(t.Context(), selected(item))
	if err == nil || len(plan.Observed) != 1 || plan.Observed[0].Item.ID != item.ID || len(plan.Observed[0].Errors) == 0 {
		t.Fatalf("missing preflight lost item: plan=%+v err=%v", plan, err)
	}
}

func TestCancellationStopsOnlyLocalWork(t *testing.T) {
	a, b := unlocked("a", "Movie"), unlocked("b", "Movie")
	f, engine := newFixture(t, []jellyfin.Item{a, b}, Options{Checks: []string{"overview"}})
	plan := mustPlan(t, engine, selected(a, b))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var events []Event
	err := engine.Run(ctx, plan, func(event Event) {
		events = append(events, event)
		if event.State == "queued" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || len(f.posts) != 1 || finalEvent(t, events, a.ID).State != "unknown" || finalEvent(t, events, b.ID).State != "skipped" {
		t.Fatalf("posts=%v events=%+v err=%v", f.posts, events, err)
	}
}

func TestConcurrentObservationsBoundNetworkWorkAndCancelWaiters(t *testing.T) {
	item := unlocked("a", "Movie")
	f, engine := newFixture(t, []jellyfin.Item{item}, Options{Checks: []string{"overview"}})
	started := make(chan struct{})
	release := make(chan struct{})
	var hits atomic.Int32
	f.intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if hits.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return false
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := engine.observe(t.Context(), selected(item))
		firstDone <- err
	}()
	<-started
	waitCtx, cancel := context.WithCancel(t.Context())
	var waiters sync.WaitGroup
	for range 20 {
		waiters.Go(func() {
			_, err := engine.observe(waitCtx, selected(item))
			if !errors.Is(err, context.Canceled) {
				t.Errorf("blocked observer did not cancel: %v", err)
			}
		})
	}
	cancel()
	waiters.Wait()
	if hits.Load() != 1 {
		t.Errorf("concurrent observations issued %d network requests", hits.Load())
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestPollingRateLimitInterruptsPendingDispatch(t *testing.T) {
	a, b := unlocked("a", "Movie"), unlocked("series", "Series")
	f, engine := newFixture(t, []jellyfin.Item{a, b}, Options{Checks: []string{"overview"}, Timeout: time.Second})
	var running atomic.Bool
	blocked := make(chan struct{})
	var once sync.Once
	f.intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if !running.Load() || r.Method != "GET" {
			return false
		}
		if r.URL.Query().Get("ParentId") == b.ID {
			once.Do(func() { close(blocked) })
			<-r.Context().Done()
			return true
		}
		if r.URL.Query().Get("Ids") == a.ID {
			// Let both POST-time validation and the first POST through; only
			// the subsequent observation gets rate limited.
			f.mu.Lock()
			posted := len(f.posts) > 0
			f.mu.Unlock()
			if posted {
				w.WriteHeader(http.StatusTooManyRequests)
				return true
			}
		}
		return false
	}
	plan := mustPlan(t, engine, selected(a, b))
	// Plan sorts Series before Movie; put the two independent actions in the
	// confirmed order that exercises a pending recursive review second.
	slices.Reverse(plan.Actions)
	running.Store(true)
	events, err := collectRun(t, engine, plan)
	if !jellyfin.IsRateLimitError(err) || len(f.posts) != 1 || f.posts[0].id != a.ID || finalEvent(t, events, b.ID).State != "skipped" {
		t.Fatalf("poll failure did not stop dispatch: posts=%v events=%+v err=%v", f.posts, events, err)
	}
}
