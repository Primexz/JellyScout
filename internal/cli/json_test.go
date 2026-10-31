package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Primexz/jellyscout/internal/jellyfin"
)

func TestTerminalIncludesJellyfinItemLinks(t *testing.T) {
	f := &fixture{items: map[string][]jellyfin.Item{"movies": {{ID: "broken", Type: "Movie", Name: "Needs artwork"}}}}
	baseURL := f.serve(t)
	code, out, errout := invoke(t.Context(), baseURL, "scan")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	for _, want := range []string{baseURL, baseURL + "/web/index.html#/details?id=broken&serverId=server"} {
		if !strings.Contains(out, want) {
			t.Errorf("terminal output is missing Jellyfin URL %q", want)
		}
	}
}

func TestScanJSONIsCleanAndIncludesURLs(t *testing.T) {
	f := &fixture{items: map[string][]jellyfin.Item{"movies": {
		{ID: "broken", Type: "Movie", Name: "Needs artwork"},
		healthy("healthy", "Movie", "Healthy movie"),
	}}}
	baseURL := f.serve(t)
	code, out, errout := invoke(t.Context(), baseURL, "scan", "--format", "json", "--fail-on-issues")
	if code != 1 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	var doc struct {
		SchemaVersion int `json:"schema_version"`
		Server        struct {
			URL string `json:"url"`
		} `json:"server"`
		Complete bool                                     `json:"complete"`
		Summary  struct{ Scanned, Healthy, Affected int } `json:"summary"`
		Items    []struct{ ID, URL string }               `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out)
	}
	if doc.SchemaVersion != 1 || doc.Server.URL != baseURL || !doc.Complete || doc.Summary.Scanned != 2 || doc.Summary.Healthy != 1 || doc.Summary.Affected != 1 {
		t.Fatalf("incorrect JSON overview: %+v", doc)
	}
	if len(doc.Items) != 1 || doc.Items[0].ID != "broken" || doc.Items[0].URL != baseURL+"/web/index.html#/details?id=broken&serverId=server" {
		t.Fatalf("incorrect affected items: %+v", doc.Items)
	}
	if strings.Contains(out, "cli-secret") || !strings.Contains(errout, "Scanning library") {
		t.Fatal("credentials leaked or progress was not sent to stderr")
	}
}

func TestLibrariesJSONIncludesServerURL(t *testing.T) {
	f := &fixture{items: map[string][]jellyfin.Item{}}
	baseURL := f.serve(t)
	code, out, errout := invoke(t.Context(), baseURL, "libraries", "--format", "json")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	var doc struct {
		Server struct {
			URL string `json:"url"`
		} `json:"server"`
		Libraries []struct {
			ID string `json:"id"`
		} `json:"libraries"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Server.URL != baseURL || len(doc.Libraries) != 2 || doc.Libraries[0].ID != "movies" {
		t.Fatalf("unexpected libraries JSON: %+v", doc)
	}
	code, out, errout = invoke(t.Context(), baseURL, "libraries")
	if code != 0 || !strings.Contains(out, baseURL) {
		t.Fatalf("library output missing URL: code=%d stdout=%s stderr=%s", code, out, errout)
	}
}

func TestJSONVerificationFailureRemainsParseable(t *testing.T) {
	f := &fixture{items: map[string][]jellyfin.Item{"movies": {healthy("movie", "Movie", "Image unavailable")}},
		imageHandler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) },
	}
	code, out, errout := invoke(t.Context(), f.serve(t), "scan", "--format", "json", "--verify-images", "--checks", "primary-image")
	var doc struct {
		Complete bool `json:"complete"`
		Summary  struct {
			OperationalErrors int `json:"operational_errors"`
		} `json:"summary"`
		Items []struct {
			Errors []string `json:"errors"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("invalid incomplete report: %v\n%s", err, out)
	}
	if code != 2 || doc.Complete || doc.Summary.OperationalErrors != 1 || len(doc.Items) != 1 || len(doc.Items[0].Errors) != 1 {
		t.Fatalf("code=%d doc=%+v stderr=%s", code, doc, errout)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestJSONStdoutWriteFailure(t *testing.T) {
	f := &fixture{items: map[string][]jellyfin.Item{}}
	baseURL := f.serve(t)
	getenv := func(key string) string {
		switch key {
		case "JELLYFIN_URL":
			return baseURL
		case "JELLYFIN_API_KEY":
			return "cli-secret"
		}
		return ""
	}
	for _, command := range []string{"scan", "libraries"} {
		var stderr bytes.Buffer
		if code := Run(t.Context(), []string{command, "--format", "json"}, brokenWriter{}, &stderr, getenv); code != 2 {
			t.Fatalf("command=%s code=%d stderr=%s", command, code, &stderr)
		}
	}
}
