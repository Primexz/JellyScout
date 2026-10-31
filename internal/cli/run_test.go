package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Primexz/jellyscout/internal/jellyfin"
)

type fixture struct {
	items        map[string][]jellyfin.Item
	itemStatus   int
	serverStatus int
	imageHandler http.HandlerFunc
	mu           sync.Mutex
	queries      []string
	imageHits    atomic.Int32
}

func (f *fixture) querySnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.queries)
}

func (f *fixture) serve(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != `MediaBrowser Token="cli-secret"` {
			t.Errorf("missing auth: %v", r.Header)
		}
		switch {
		case r.URL.Path == "/jf/System/Info":
			if f.serverStatus != 0 {
				w.WriteHeader(f.serverStatus)
				if _, err := fmt.Fprint(w, "cli-secret"); err != nil {
					t.Errorf("write server error response: %v", err)
				}
				return
			}
			if _, err := fmt.Fprint(w, `{"Id":"server","ServerName":"Home Cinema","Version":"10.11"}`); err != nil {
				t.Errorf("write server info response: %v", err)
			}
		case r.URL.Path == "/jf/Library/MediaFolders":
			if _, err := fmt.Fprint(w, `{"Items":[{"Id":"movies","Name":"Movies","CollectionType":"movies"},{"Id":"tv","Name":"TV","CollectionType":"tvshows"}]}`); err != nil {
				t.Errorf("write libraries response: %v", err)
			}
		case r.URL.Path == "/jf/Items":
			f.mu.Lock()
			f.queries = append(f.queries, r.URL.RawQuery)
			f.mu.Unlock()
			if f.itemStatus != 0 {
				w.WriteHeader(f.itemStatus)
				if _, err := fmt.Fprint(w, "cli-secret"); err != nil {
					t.Errorf("write items error response: %v", err)
				}
				return
			}
			wanted := strings.Split(r.URL.Query().Get("IncludeItemTypes"), ",")
			items := make([]jellyfin.Item, 0)
			for _, item := range f.items[r.URL.Query().Get("ParentId")] {
				if slices.Contains(wanted, item.Type) {
					items = append(items, item)
				}
			}
			if err := json.NewEncoder(w).Encode(map[string]any{"Items": items, "TotalRecordCount": len(items)}); err != nil {
				t.Errorf("encode items response: %v", err)
			}
		case strings.Contains(r.URL.Path, "/Images/"):
			f.imageHits.Add(1)
			if f.imageHandler != nil {
				f.imageHandler(w, r)
			} else {
				w.WriteHeader(404)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s.URL + "/jf"
}

func healthy(id, kind, name string) jellyfin.Item {
	return jellyfin.Item{ID: id, Type: kind, Name: name, Overview: "A complete synopsis.", ProductionYear: new(2024), Genres: []string{"Drama"}, ProviderIDs: map[string]string{"Tmdb": "42"}, ImageTags: map[string]string{"Primary": "poster"}, BackdropImageTags: []string{"backdrop"}, IndexNumber: new(0), ParentIndexNumber: new(0)}
}

func invoke(ctx context.Context, baseURL string, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	getenv := func(name string) string {
		switch name {
		case "JELLYFIN_URL":
			return baseURL
		case "JELLYFIN_API_KEY":
			return "cli-secret"
		}
		return ""
	}
	code := Run(ctx, args, &stdout, &stderr, getenv)
	return code, stdout.String(), stderr.String()
}

func TestScanDefaultAndJSONFile(t *testing.T) {
	f := &fixture{items: map[string][]jellyfin.Item{
		"movies": {{ID: "broken", Type: "Movie", Name: "The Missing Poster"}},
		"tv":     {healthy("show", "Series", "Healthy Show"), {ID: "season", Type: "Season", Name: "Excluded Season"}, {ID: "episode", Type: "Episode", Name: "Excluded Episode"}},
	}}
	baseURL := f.serve(t)
	path := filepath.Join(t.TempDir(), "report.json")
	code, out, errout := invoke(t.Context(), baseURL, "scan", "--json", path)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	if !strings.Contains(out, "The Missing Poster") || strings.Contains(out, "Excluded Season") || strings.Contains(out, "Excluded Episode") {
		t.Fatalf("unexpected report:\n%s", out)
	}
	if f.imageHits.Load() != 0 {
		t.Fatal("default scan fetched images")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("The Missing Poster")) || bytes.Contains(body, []byte("cli-secret")) {
		t.Fatal("missing item or leaked key in JSON")
	}
	if !json.Valid(body) {
		t.Fatalf("saved report is not JSON: %s", body)
	}
	if !bytes.Contains(body, []byte("/jf/web/index.html")) {
		t.Fatal("missing base-path-aware item link")
	}
	code, _, errout = invoke(t.Context(), baseURL, "scan", "--fail-on-issues")
	if code != 1 {
		t.Fatalf("fail-on-issues code=%d stderr=%s", code, errout)
	}
	code, _, errout = invoke(t.Context(), baseURL, "scan", "--checks", "title", "--fail-on-issues")
	if code != 0 {
		t.Fatalf("restrict checks code=%d stderr=%s", code, errout)
	}
}

func TestTVSelectionPlaceholdersAndDeduplication(t *testing.T) {
	virtual := healthy("virtual", "Episode", "Virtual Episode")
	virtual.LocationType = "Virtual"
	placeholder := healthy("placeholder", "Episode", "Placeholder Episode")
	placeholder.IsPlaceHolder = true
	season := jellyfin.Item{ID: "season", Type: "Season", Name: "Specials", IndexNumber: new(0)}
	episode := healthy("special", "Episode", "Real Special")
	episode.SeriesName = "A Show"
	episode.SeriesID = "show"
	duplicate := healthy("duplicate", "Movie", "Duplicate Film")
	duplicate.Overview = ""
	f := &fixture{items: map[string][]jellyfin.Item{
		"movies": {duplicate},
		"tv":     {season, episode, virtual, placeholder, duplicate},
	}}
	baseURL := f.serve(t)
	code, _, errout := invoke(t.Context(), baseURL, "scan", "--library", "tv", "--library", "tv", "--include-seasons", "--include-episodes", "--checks", "numbering", "--fail-on-issues")
	if code != 0 {
		t.Fatalf("valid specials code=%d stderr=%s", code, errout)
	}
	queries := f.querySnapshot()
	if len(queries) != 1 || strings.Contains(queries[0], "ParentId=movies") {
		t.Fatalf("queries=%v", queries)
	}
	code, _, errout = invoke(t.Context(), baseURL, "scan", "--include-episodes", "--checks", "title")
	if code != 0 {
		t.Fatalf("episode-only inclusion code=%d stderr=%s", code, errout)
	}
	queries = f.querySnapshot()
	if !strings.Contains(queries[1], "Movie%2CSeries%2CEpisode") || strings.Contains(queries[1], "Season") {
		t.Fatalf("independent flags query=%s", queries[1])
	}
	// Inspect collected results directly via visible failures: each duplicate title
	// should appear only once even if an item belongs to overlapping libraries.
	code, out, errout := invoke(t.Context(), baseURL, "scan", "--checks", "overview")
	if code != 0 || strings.Count(out, "Duplicate Film") != 1 {
		t.Fatalf("deduplication: code=%d out=%s stderr=%s", code, out, errout)
	}
	if strings.Contains(out, "Virtual Episode") || strings.Contains(out, "Placeholder Episode") {
		t.Fatalf("placeholder leaked into report: %s", out)
	}
}

func TestMetadataAndAuthFailuresPreserveJSONFile(t *testing.T) {
	for _, mode := range []string{"metadata", "auth", "image auth", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f := &fixture{items: map[string][]jellyfin.Item{"movies": {healthy("movie", "Movie", "Movie")}}}
			ctx := t.Context()
			args := []string{"scan"}
			switch mode {
			case "metadata":
				f.itemStatus = 500
			case "auth":
				f.serverStatus = 401
			case "image auth":
				args = append(args, "--verify-images")
				f.imageHandler = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			baseURL := f.serve(t)
			path := filepath.Join(t.TempDir(), "report.json")
			if err := os.WriteFile(path, []byte("existing report"), 0600); err != nil {
				t.Fatal(err)
			}
			code, _, errout := invoke(ctx, baseURL, append(args, "--json", path)...)
			if code != 2 || strings.Contains(errout, "cli-secret") {
				t.Fatalf("code=%d stderr=%s", code, errout)
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != "existing report" {
				t.Fatalf("existing JSON overwritten: %q", body)
			}
		})
	}
}

func TestImageFindingsAndIncompleteReports(t *testing.T) {
	var pngBody bytes.Buffer
	if err := png.Encode(&pngBody, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	for _, operational := range []bool{false, true} {
		t.Run(fmt.Sprintf("operational=%v", operational), func(t *testing.T) {
			movie := healthy("movie", "Movie", "An Image Problem")
			movie.BackdropImageTags = []string{"good", "bad"}
			f := &fixture{items: map[string][]jellyfin.Item{"movies": {movie}}, imageHandler: func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/Primary/"):
					if operational {
						w.WriteHeader(500)
					} else {
						w.WriteHeader(404)
					}
				case strings.HasSuffix(r.URL.Path, "/Backdrop/0"):
					if _, err := w.Write(pngBody.Bytes()); err != nil {
						t.Errorf("write PNG response: %v", err)
					}
				default:
					if _, err := w.Write([]byte("invalid image")); err != nil {
						t.Errorf("write invalid image response: %v", err)
					}
				}
			}}
			path := filepath.Join(t.TempDir(), "report.json")
			code, out, errout := invoke(t.Context(), f.serve(t), "scan", "--verify-images", "--json", path, "--fail-on-issues")
			wantCode := 1
			if operational {
				wantCode = 2
			}
			if code != wantCode || f.imageHits.Load() != 3 {
				t.Fatalf("code=%d hits=%d out=%s err=%s", code, f.imageHits.Load(), out, errout)
			}
			if !strings.Contains(out, "Backdrop 1") {
				t.Fatalf("did not verify second backdrop: %s", out)
			}
			if operational && !strings.Contains(strings.ToLower(out), "incomplete") {
				t.Fatalf("no incomplete warning: %s", out)
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(body, []byte("cli-secret")) {
				t.Fatal("key leaked")
			}
			var doc struct {
				Complete bool `json:"complete"`
			}
			if err := json.Unmarshal(body, &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Complete == operational {
				t.Fatalf("incorrect report completeness: %s", body)
			}
		})
	}
}

func TestImageConcurrencyLimit(t *testing.T) {
	var active, maximum atomic.Int32
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	f := &fixture{items: map[string][]jellyfin.Item{}, imageHandler: func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old; old = maximum.Load() {
			if maximum.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		if _, err := w.Write(b.Bytes()); err != nil {
			t.Errorf("write PNG response: %v", err)
		}
	}}
	for i := range 8 {
		f.items["movies"] = append(f.items["movies"], healthy(fmt.Sprintf("movie%d", i), "Movie", fmt.Sprintf("Movie %d", i)))
	}
	code, _, errout := invoke(t.Context(), f.serve(t), "scan", "--verify-images", "--checks", "primary-image", "--concurrency", "2")
	if code != 0 || f.imageHits.Load() != 8 || maximum.Load() != 2 {
		t.Fatalf("code=%d hits=%d max=%d stderr=%s", code, f.imageHits.Load(), maximum.Load(), errout)
	}
}

func TestCancellationDuringImageVerification(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	f := &fixture{items: map[string][]jellyfin.Item{"movies": {healthy("movie", "Movie", "Movie")}}, imageHandler: func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-r.Context().Done()
	}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		select {
		case <-entered:
			cancel()
		case <-ctx.Done():
		}
	}()
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, []byte("previous report"), 0600); err != nil {
		t.Fatal(err)
	}
	code, _, errout := invoke(ctx, f.serve(t), "scan", "--verify-images", "--json", path)
	if code != 2 {
		t.Fatalf("cancelled scan code=%d stderr=%s", code, errout)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "previous report" {
		t.Fatalf("existing report replaced: %q err=%v", body, err)
	}
}

func TestHelpAndInvalidArguments(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want int
	}{
		{nil, 0}, {[]string{"--help"}, 0}, {[]string{"scan", "--help"}, 0}, {[]string{"libraries", "--help"}, 0},
		{[]string{"unknown"}, 2}, {[]string{"scan", "--checks", ""}, 2}, {[]string{"scan", "--checks", "typo"}, 2},
		{[]string{"scan", "--concurrency", "0"}, 2}, {[]string{"scan", "--timeout", "0s"}, 2},
		{[]string{"scan", "--library", ""}, 2}, {[]string{"scan", "extra"}, 2}, {[]string{"scan", "--unknown"}, 2},
		{[]string{"scan", "--format", "xml"}, 2}, {[]string{"libraries", "--format", "html"}, 2},
		{[]string{"scan", "--html", "obsolete.html"}, 2},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			code, out, errout := invoke(t.Context(), "", tt.args...)
			if code != tt.want {
				t.Fatalf("code=%d want=%d", code, tt.want)
			}
			if strings.Contains(out+errout, "cli-secret") {
				t.Fatal("API key leaked through help or error output")
			}
		})
	}
}

func TestLibrariesSelectionAndJSONWriteFailure(t *testing.T) {
	f := &fixture{items: map[string][]jellyfin.Item{}}
	baseURL := f.serve(t)
	code, out, errout := invoke(t.Context(), baseURL, "libraries")
	if code != 0 || !strings.Contains(out, "movies") || !strings.Contains(out, "TV") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errout)
	}
	code, _, errout = invoke(t.Context(), baseURL, "scan", "--library", "unknown")
	if code != 2 || !strings.Contains(errout, "unknown library") {
		t.Fatalf("code=%d err=%s", code, errout)
	}
	code, _, errout = invoke(t.Context(), baseURL, "scan", "--json", filepath.Join(t.TempDir(), "missing", "report.json"))
	if code != 2 || !strings.Contains(errout, "Write JSON") {
		t.Fatalf("code=%d err=%s", code, errout)
	}
}
