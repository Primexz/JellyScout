package scan

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Primexz/jellyscout/internal/audit"
	"github.com/Primexz/jellyscout/internal/jellyfin"
)

func testClient(t *testing.T, handler http.HandlerFunc) *jellyfin.Client {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	c, err := jellyfin.New(s.URL, "scan-secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestCheckResultsReplacesFindingsWithoutChangingInput(t *testing.T) {
	input := []audit.Result{{
		Library: jellyfin.Library{ID: "lib", Name: "Movies"},
		Item:    jellyfin.Item{ID: "movie", Type: "Movie", ImageTags: map[string]string{"Primary": "poster"}},
		URL:     "https://example.test/item", Findings: []audit.Finding{{Check: "overview", Message: "old finding"}},
		Errors: []string{"old error"},
	}}
	before := slices.Clone(input)
	checked, err := CheckResults(t.Context(), nil, input, []string{"title"}, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(input, before) {
		t.Fatal("input results were mutated")
	}
	if len(checked) != 1 || checked[0].Library != input[0].Library || checked[0].URL != input[0].URL || !reflect.DeepEqual(checked[0].Item, input[0].Item) {
		t.Fatalf("lost result identity: %+v", checked)
	}
	if len(checked[0].Errors) != 0 || len(checked[0].Findings) != 1 || checked[0].Findings[0].Check != "title" {
		t.Fatalf("stale checks remain: %+v", checked[0])
	}
	checked[0].Findings[0].Message = "changed"
	checked[0].URL = "changed"
	if !reflect.DeepEqual(input, before) {
		t.Fatal("result slices share writable storage")
	}
	cleared, err := CheckResults(t.Context(), nil, input, nil, false, 0)
	if err != nil || len(cleared[0].Findings) != 0 || len(cleared[0].Errors) != 0 {
		t.Fatalf("empty check set: %+v, %v", cleared, err)
	}
}

func TestCheckResultsImageFailures(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusOK, http.StatusServiceUnavailable, http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
			input := []audit.Result{{Item: jellyfin.Item{ID: "movie", Type: "Movie", ImageTags: map[string]string{"Primary": "tag"}}}}
			checked, err := CheckResults(t.Context(), client, input, []string{"primary-image"}, true, 1)
			switch status {
			case http.StatusUnauthorized, http.StatusForbidden:
				if !jellyfin.IsAuthError(err) {
					t.Fatalf("auth error lost: %v", err)
				}
			case http.StatusTooManyRequests:
				if !jellyfin.IsRateLimitError(err) {
					t.Fatalf("rate limit error lost: %v", err)
				}
			case http.StatusServiceUnavailable:
				if err != nil || len(checked[0].Errors) != 1 || len(checked[0].Findings) != 0 {
					t.Fatalf("operational failure: %+v, %v", checked, err)
				}
			default:
				if err != nil || len(checked[0].Errors) != 0 || len(checked[0].Findings) != 1 {
					t.Fatalf("broken image: %+v, %v", checked, err)
				}
			}
			if len(input[0].Findings) != 0 || len(input[0].Errors) != 0 {
				t.Fatal("input changed")
			}
		})
	}
}

func TestCheckResultsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	})
	input := []audit.Result{{Item: jellyfin.Item{ID: "movie", Type: "Movie", ImageTags: map[string]string{"Primary": "tag"}}}}
	_, err := CheckResults(ctx, client, input, []string{"primary-image"}, true, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestRateLimitStopsImageDispatch(t *testing.T) {
	var requests atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	})
	var input []audit.Result
	for range 20 {
		input = append(input, audit.Result{Item: jellyfin.Item{ID: "movie", Type: "Movie", ImageTags: map[string]string{"Primary": "tag"}}})
	}
	_, err := CheckResults(t.Context(), client, input, []string{"primary-image"}, true, 1)
	if !jellyfin.IsRateLimitError(err) || requests.Load() != 1 {
		t.Fatalf("rate limit did not stop dispatch: err=%v requests=%d", err, requests.Load())
	}
}

func TestRunScopeValidationAndFiltering(t *testing.T) {
	var itemRequests atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body any
		switch r.URL.Path {
		case "/System/Info":
			body = jellyfin.ServerInfo{ID: "server"}
		case "/Library/MediaFolders":
			body = map[string]any{"Items": []jellyfin.Library{{ID: "lib", Name: "Movies"}, {ID: "other", Name: "Other"}}}
		case "/Items":
			itemRequests.Add(1)
			items := []jellyfin.Item{
				{ID: "movie", Type: "Movie", Name: "Movie"},
				{ID: "show", Type: "Series", Name: "Show"},
				{ID: "season", Type: "Season", Name: "Season"},
				{ID: "episode", Type: "Episode", Name: "Episode"},
				{ID: "virtual", Type: "Episode", LocationType: "Virtual"},
				{ID: "placeholder", Type: "Movie", IsPlaceHolder: true},
				{ID: "folder", Type: "Folder"},
			}
			body = map[string]any{"Items": items, "TotalRecordCount": len(items)}
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("encode: %v", err)
		}
	})
	opts := Options{LibraryIDs: []string{"missing"}, Checks: []string{"title"}, Concurrency: 2}
	if _, err := Run(t.Context(), client, opts, nil); err == nil || !strings.Contains(err.Error(), "unknown library") || itemRequests.Load() != 0 {
		t.Fatalf("unknown scope scanned: %v (%d requests)", err, itemRequests.Load())
	}
	opts.LibraryIDs = []string{"lib", "lib"}
	opts.IncludeEpisodes = true
	var progress []string
	r, err := Run(t.Context(), client, opts, func(s string) { progress = append(progress, s) })
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Results) != 3 || itemRequests.Load() != 1 || !r.Complete || r.GeneratedAt.IsZero() || len(progress) != 1 {
		t.Fatalf("wrong scope/report: %+v; requests=%d; progress=%v", r, itemRequests.Load(), progress)
	}
	for _, result := range r.Results {
		if result.Item.ID == "season" || result.Item.ID == "virtual" || result.Item.ID == "placeholder" || result.Item.ID == "folder" {
			t.Fatalf("out-of-scope item: %+v", result)
		}
		if !strings.Contains(result.URL, "serverId=server") {
			t.Fatalf("missing item URL: %+v", result)
		}
	}
	opts.LibraryIDs = nil
	opts.IncludeSeasons = true
	r, err = Run(t.Context(), client, opts, nil)
	if err != nil || len(r.Results) != 4 || itemRequests.Load() != 3 {
		t.Fatalf("cross-library deduplication: %+v, %v", r, err)
	}
}

func TestRunInvalidOptionsBeforeHTTP(t *testing.T) {
	var requests atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); http.Error(w, "unexpected", 500) })
	for _, opts := range []Options{{Checks: []string{"typo"}, Concurrency: 1}, {Checks: []string{"title"}, Concurrency: 0}} {
		if _, err := Run(t.Context(), client, opts, nil); err == nil {
			t.Fatal("invalid options accepted")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid options contacted server")
	}
}
