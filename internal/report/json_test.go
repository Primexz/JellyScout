package report

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Primexz/jellyscout/internal/audit"
	"github.com/Primexz/jellyscout/internal/jellyfin"
)

func renderJSON(t *testing.T, r audit.Report) string {
	t.Helper()
	var b bytes.Buffer
	if err := WriteJSON(&b, r); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func decodeObject(t *testing.T, data string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(data), &doc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	return doc
}

func assertKeys(t *testing.T, object map[string]any, want ...string) {
	t.Helper()
	var got []string
	for key := range object {
		got = append(got, key)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("JSON keys = %v, want %v", got, want)
	}
}

func TestJSONReportSchemaAndContent(t *testing.T) {
	r := representativeReport()
	r.Results[0].Item.Overview = "private overview"
	r.Results[0].Item.ProviderIDs = map[string]string{"secret-provider": "provider-value"}
	r.Results[0].Item.ImageTags = map[string]string{"Primary": "private-image-tag"}
	r.Results[0].Item.BackdropImageTags = []string{"private-backdrop-tag"}
	output := renderJSON(t, r)
	doc := decodeObject(t, output)
	assertKeys(t, doc, "schema_version", "server", "generated_at", "complete", "verify_images", "checks", "summary", "items")
	if doc["schema_version"] != float64(1) || doc["complete"] != true || doc["verify_images"] != true {
		t.Errorf("incorrect version/status: %#v", doc)
	}
	if got := doc["generated_at"]; got != "2026-10-31T12:34:56Z" {
		t.Errorf("generated_at = %v", got)
	}
	wantServer := map[string]any{"id": "server-1", "name": "Home Jellyfin", "version": "10.11.0", "url": r.ServerURL}
	if !reflect.DeepEqual(doc["server"], wantServer) {
		t.Errorf("server = %#v, want %#v", doc["server"], wantServer)
	}
	if !reflect.DeepEqual(doc["checks"], []any{"overview", "genres", "primary-image", "provider-ids"}) {
		t.Errorf("checks changed: %#v", doc["checks"])
	}
	wantSummary := decodeObject(t, `{
		"scanned":8,"healthy":3,"affected":5,"findings":5,"operational_errors":2,
		"by_library":[
			{"id":"docs","name":"Documentaries","scanned":1,"affected":0},
			{"id":"movies","name":"Movies","scanned":2,"affected":1},
			{"id":"tv","name":"Television","scanned":5,"affected":4}],
		"by_type":[
			{"type":"Episode","scanned":2,"affected":2},
			{"type":"Movie","scanned":3,"affected":1},
			{"type":"Season","scanned":1,"affected":1},
			{"type":"Series","scanned":2,"affected":1}],
		"by_check":[
			{"check":"genres","findings":2},{"check":"overview","findings":2},
			{"check":"primary-image","findings":1},{"check":"provider-ids","findings":0}]
	}`)
	if !reflect.DeepEqual(doc["summary"], wantSummary) {
		t.Errorf("summary = %#v, want %#v", doc["summary"], wantSummary)
	}
	items := doc["items"].([]any)
	var ids []string
	for _, raw := range items {
		item := raw.(map[string]any)
		ids = append(ids, item["id"].(string))
		for _, field := range []string{"findings", "errors"} {
			if _, ok := item[field].([]any); !ok {
				t.Errorf("item %v has non-array %s: %#v", item["id"], field, item[field])
			}
		}
	}
	if !slices.Equal(ids, []string{"movie-1", "episode-1", "series-1", "episode-error", "season-0"}) {
		t.Fatalf("affected item order = %v", ids)
	}
	movie := items[0].(map[string]any)
	assertKeys(t, movie, "id", "type", "name", "title", "library", "url", "path", "findings", "errors")
	if movie["name"] != "The Quiet City" || movie["title"] != "The Quiet City (2024)" || movie["path"] != r.Results[0].Item.Path || movie["url"] != r.Results[0].URL {
		t.Errorf("movie context missing: %#v", movie)
	}
	if !reflect.DeepEqual(movie["library"], map[string]any{"id": "movies", "name": "Movies"}) {
		t.Errorf("item library = %#v", movie["library"])
	}
	finding := movie["findings"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(finding, map[string]any{"check": "overview", "message": "Overview is missing"}) {
		t.Errorf("finding = %#v", finding)
	}
	episode := items[1].(map[string]any)
	if episode["title"] != "Northern Light · Season 2 · S02E03 · Arrival" || episode["series_name"] != "Northern Light" || episode["season_name"] != "Season 2" || episode["season_number"] != float64(2) || episode["episode_number"] != float64(3) {
		t.Errorf("episode context missing: %#v", episode)
	}
	if season := items[4].(map[string]any); season["season_number"] != float64(0) {
		t.Errorf("special season number omitted: %#v", season)
	}
	for _, forbidden := range []string{"ProviderIds", "ImageTags", "BackdropImageTags", `"Overview"`, "private overview", "secret-provider", "provider-value", "private-image-tag", "private-backdrop-tag"} {
		if strings.Contains(output, forbidden) {
			t.Errorf("JSON exposed raw API data %q", forbidden)
		}
	}
}

func TestJSONReportStatusesAndEmptyArrays(t *testing.T) {
	for _, tt := range []struct {
		name     string
		report   audit.Report
		scanned  int
		healthy  int
		affected int
	}{
		{name: "empty incomplete"},
		{name: "empty complete", report: audit.Report{Complete: true}},
		{name: "partial", report: func() audit.Report { r := representativeReport(); r.Complete = false; return r }(), scanned: 8, healthy: 3, affected: 5},
		{name: "all healthy", report: audit.Report{Complete: true, Results: []audit.Result{{Item: jellyfin.Item{Name: "Healthy", Type: "Movie"}}}}, scanned: 1, healthy: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			doc := decodeObject(t, renderJSON(t, tt.report))
			if doc["complete"] != tt.report.Complete || doc["verify_images"] != tt.report.VerifyImages {
				t.Errorf("status changed: %#v", doc)
			}
			if _, err := time.Parse(time.RFC3339, doc["generated_at"].(string)); err != nil {
				t.Errorf("generated_at is not RFC3339: %v", err)
			}
			for _, field := range []string{"items", "checks"} {
				if _, ok := doc[field].([]any); !ok {
					t.Errorf("%s must be an array: %#v", field, doc[field])
				}
			}
			summary := doc["summary"].(map[string]any)
			for field, want := range map[string]int{"scanned": tt.scanned, "healthy": tt.healthy, "affected": tt.affected} {
				if summary[field] != float64(want) {
					t.Errorf("%s = %v, want %d", field, summary[field], want)
				}
			}
			for _, field := range []string{"by_library", "by_type", "by_check"} {
				if _, ok := summary[field].([]any); !ok {
					t.Errorf("%s must be an array: %#v", field, summary[field])
				}
			}
			if len(doc["items"].([]any)) != tt.affected {
				t.Error("healthy or missing items in details")
			}
		})
	}
}

func TestJSONPreservesEscapingAndControlsInMetadata(t *testing.T) {
	const text = "quote\" slash\\ <tag>& 日本語\n\r\t\x00\x1b[31m\u009b\u202e"
	r := audit.Report{
		Server: jellyfin.ServerInfo{ID: text, ServerName: text, Version: text}, Checks: []string{text},
		Results: []audit.Result{{
			Library:  jellyfin.Library{ID: text, Name: text},
			Item:     jellyfin.Item{ID: text, Type: "Episode", Name: text, SeriesName: text, SeasonName: "season" + text, Path: text},
			Findings: []audit.Finding{{Check: text, Message: text}}, Errors: []string{text},
		}},
	}
	doc := decodeObject(t, renderJSON(t, r))
	server := doc["server"].(map[string]any)
	item := doc["items"].([]any)[0].(map[string]any)
	finding := item["findings"].([]any)[0].(map[string]any)
	for field, got := range map[string]any{
		"server id": server["id"], "server name": server["name"], "version": server["version"],
		"check": doc["checks"].([]any)[0], "id": item["id"], "name": item["name"], "path": item["path"],
		"library id": item["library"].(map[string]any)["id"], "library name": item["library"].(map[string]any)["name"],
		"finding check": finding["check"], "message": finding["message"], "error": item["errors"].([]any)[0], "series": item["series_name"],
	} {
		if got != text {
			t.Errorf("%s changed during JSON round trip: %q", field, got)
		}
	}
	if item["title"] != text+" · season"+text+" · "+text {
		t.Errorf("context title changed: %q", item["title"])
	}
}

func TestJSONAndTerminalURLsPreserveBasePaths(t *testing.T) {
	r := representativeReport()
	r.ServerURL = "https://jellyfin.example/media/jellyfin"
	r.Results = r.Results[:1]
	r.Results[0].URL = r.ServerURL + "/web/#/details?id=movie%201&serverId=server-1"
	doc := decodeObject(t, renderJSON(t, r))
	if doc["server"].(map[string]any)["url"] != r.ServerURL || doc["items"].([]any)[0].(map[string]any)["url"] != r.Results[0].URL {
		t.Error("JSON changed server/item URLs")
	}
	var b bytes.Buffer
	if err := WriteTerminal(&b, r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "\nJellyfin URL: "+r.ServerURL+"\n") || !strings.Contains(b.String(), "\n  Jellyfin: "+r.Results[0].URL+"\n") {
		t.Errorf("terminal omitted URLs with base path: %s", b.String())
	}
}

func TestUnsafeURLsAreOmitted(t *testing.T) {
	for _, raw := range []string{
		"javascript:alert(1)", "JaVaScRiPt:alert(1)", "data:text/html,attack", "file:///etc/passwd",
		"//evil.example/path", "/web/#/details?id=1", "https://", "https://%zz", "https://example.org:invalid/",
		"https://user:secret@example.org/path", "https://example.org\\@evil.example/", "https://example.org/\nattack",
		"https://example.org/\u009b31m", "https://example.org/\x1b]52;c;payload\a", "https://example.org/\u202eattack",
		"mailto:someone@example.org", "https://example.org/with space",
		"https://example.org?api_key=secret", "https://example.org?X-Emby-Token=secret",
		"https://example.org/web/#/details?id=1&access_token=secret",
	} {
		t.Run(raw, func(t *testing.T) {
			r := representativeReport()
			r.Results = r.Results[:1]
			r.ServerURL, r.Results[0].URL = raw, raw
			var b bytes.Buffer
			if err := WriteTerminal(&b, r); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(b.String(), "\n  Jellyfin:") || !strings.Contains(b.String(), "\nJellyfin URL: \n") || strings.Contains(b.String(), raw) {
				t.Errorf("unsafe URL appeared in terminal: %q", b.String())
			}
			doc := decodeObject(t, renderJSON(t, r))
			if doc["server"].(map[string]any)["url"] != "" || doc["items"].([]any)[0].(map[string]any)["url"] != "" {
				t.Errorf("unsafe URL retained in JSON: %#v", doc)
			}
		})
	}
	for _, raw := range []string{"http://localhost:8096/web/#/details?id=abc", "HTTPS://example.org/web/"} {
		if got := safeURL(raw); got != raw {
			t.Errorf("safe URL rejected: %q", raw)
		}
	}
}

func TestLibrariesJSON(t *testing.T) {
	server := jellyfin.ServerInfo{ID: "server", ServerName: "Home", Version: "10.11"}
	const serverURL = "https://jellyfin.example/base/path"
	libs := []jellyfin.Library{{ID: "tv", Name: "Shows\n\x1b[31m\"", CollectionType: "tvshows"}, {ID: "movies", Name: "Films", CollectionType: "movies"}}
	for _, libraries := range [][]jellyfin.Library{libs, nil, {}} {
		var b bytes.Buffer
		if err := WriteLibrariesJSON(&b, server, serverURL, libraries); err != nil {
			t.Fatal(err)
		}
		doc := decodeObject(t, b.String())
		assertKeys(t, doc, "schema_version", "server", "libraries")
		if doc["schema_version"] != float64(1) || !reflect.DeepEqual(doc["server"], map[string]any{"id": "server", "name": "Home", "version": "10.11", "url": serverURL}) {
			t.Errorf("library server context changed: %#v", doc)
		}
		entries, ok := doc["libraries"].([]any)
		if !ok || len(entries) != len(libraries) {
			t.Fatalf("libraries must be an array with %d entries: %#v", len(libraries), doc["libraries"])
		}
		for i, raw := range entries {
			if !reflect.DeepEqual(raw, map[string]any{"id": libraries[i].ID, "name": libraries[i].Name, "collection_type": libraries[i].CollectionType}) {
				t.Errorf("library order/fields changed: %#v", raw)
			}
		}
	}
	var b bytes.Buffer
	if err := WriteLibrariesJSON(&b, server, "https://user:secret@example.org", nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "secret") || decodeObject(t, b.String())["server"].(map[string]any)["url"] != "" {
		t.Error("library JSON retained URL credentials")
	}
}

func TestJSONEncodingFailurePreservesDestination(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "existing"}[existing], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "report.json")
			if existing {
				if err := os.WriteFile(path, []byte("old report"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			r := representativeReport()
			r.GeneratedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			if err := WriteJSON(io.Discard, r); err == nil {
				t.Fatal("expected invalid RFC3339 year to fail encoding")
			}
			if err := WriteJSONFile(path, r); err == nil {
				t.Fatal("expected JSON file encoding failure")
			}
			if existing {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != "old report" {
					t.Fatalf("encoding failure damaged old report: %q, %v", got, err)
				}
				assertDirectoryEntries(t, dir, []string{"report.json"})
			} else {
				assertDirectoryEntries(t, dir, nil)
			}
		})
	}
}
