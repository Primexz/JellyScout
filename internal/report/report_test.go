package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/Primexz/jellyscout/internal/audit"
	"github.com/Primexz/jellyscout/internal/jellyfin"
)

func representativeReport() audit.Report {
	movies := jellyfin.Library{ID: "movies", Name: "Movies", CollectionType: "movies"}
	tv := jellyfin.Library{ID: "tv", Name: "Television", CollectionType: "tvshows"}
	docs := jellyfin.Library{ID: "docs", Name: "Documentaries", CollectionType: "movies"}
	return audit.Report{
		Server:    jellyfin.ServerInfo{ID: "server-1", ServerName: "Home Jellyfin", Version: "10.11.0"},
		ServerURL: "https://jellyfin.example", GeneratedAt: time.Date(2026, 10, 31, 12, 34, 56, 0, time.UTC),
		Complete: true, VerifyImages: true, Checks: []string{"overview", "genres", "primary-image", "provider-ids"},
		Results: []audit.Result{
			{Library: movies, Item: jellyfin.Item{ID: "movie-1", Type: "Movie", Name: "The Quiet City", ProductionYear: new(2024), Path: "/media/movies/The Quiet City.mkv"}, URL: "https://jellyfin.example/web/#/details?id=movie-1&serverId=server-1", Findings: []audit.Finding{{Check: "overview", Message: "Overview is missing"}, {Check: "genres", Message: "Genres are missing"}}},
			{Library: movies, Item: jellyfin.Item{ID: "movie-healthy", Type: "Movie", Name: "Healthy movie"}},
			{Library: tv, Item: jellyfin.Item{ID: "episode-1", Type: "Episode", Name: "Arrival", SeriesName: "Northern Light", SeasonName: "Season 2", ParentIndexNumber: new(2), IndexNumber: new(3), Path: "/media/tv/Northern Light/02/03.mkv"}, URL: "https://jellyfin.example/web/#/details?id=episode-1&serverId=server-1", Findings: []audit.Finding{{Check: "primary-image", Message: "Primary image is missing"}}, Errors: []string{"Image verification returned HTTP 503"}},
			{Library: tv, Item: jellyfin.Item{ID: "series-1", Type: "Series", Name: "Northern Light"}, Findings: []audit.Finding{{Check: "overview", Message: "Series overview is missing"}}},
			{Library: tv, Item: jellyfin.Item{ID: "episode-error", Type: "Episode", Name: "After the Storm", SeriesName: "Northern Light", SeasonName: "Season 2", ParentIndexNumber: new(2), IndexNumber: new(4)}, Errors: []string{"Timeout verifying backdrop image"}},
			{Library: tv, Item: jellyfin.Item{ID: "series-healthy", Type: "Series", Name: "Healthy show"}},
			{Library: docs, Item: jellyfin.Item{ID: "doc-healthy", Type: "Movie", Name: "Healthy documentary"}},
			{Library: tv, Item: jellyfin.Item{ID: "season-0", Type: "Season", Name: "Specials", SeriesName: "Northern Light", IndexNumber: new(0)}, Findings: []audit.Finding{{Check: "genres", Message: "Season genres are missing"}}},
		},
	}
}

func TestSummaryIncludesHealthyAndErrorOnlyItems(t *testing.T) {
	r := representativeReport()
	v := makeView(r)
	if got := []int{v.Scanned, v.Healthy, v.Affected, v.Findings, v.Errors}; !slices.Equal(got, []int{8, 3, 5, 5, 2}) {
		t.Fatalf("totals (scanned, healthy, affected, findings, errors) = %v", got)
	}
	wantChecks := []checkCount{{"genres", 2}, {"overview", 2}, {"primary-image", 1}, {"provider-ids", 0}}
	if !reflect.DeepEqual(v.Checks, wantChecks) {
		t.Errorf("checks = %#v, want %#v", v.Checks, wantChecks)
	}
	wantLibraries := []group{
		{Name: "Documentaries", ID: "docs", Scanned: 1},
		{Name: "Movies", ID: "movies", Scanned: 2, Affected: 1},
		{Name: "Television", ID: "tv", Scanned: 5, Affected: 4},
	}
	if !reflect.DeepEqual(v.Libraries, wantLibraries) {
		t.Errorf("library totals = %#v, want %#v", v.Libraries, wantLibraries)
	}
	wantTypes := []group{
		{Name: "Episode", Scanned: 2, Affected: 2},
		{Name: "Movie", Scanned: 3, Affected: 1},
		{Name: "Season", Scanned: 1, Affected: 1},
		{Name: "Series", Scanned: 2, Affected: 1},
	}
	if !reflect.DeepEqual(v.Types, wantTypes) {
		t.Errorf("type totals = %#v, want %#v", v.Types, wantTypes)
	}
	var terminal bytes.Buffer
	if err := WriteTerminal(&terminal, r); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Scanned: 8 | Healthy: 3 | Affected: 5", "Metadata findings: 5 | Operational verification errors: 2",
		"Movies [movies]: 2 / 1", "Television [tv]: 5 / 4", "Documentaries [docs]: 1 / 0",
		"Episode: 2 / 2", "Movie: 3 / 1", "Season: 1 / 1", "Series: 2 / 1",
		"genres: 2", "overview: 2", "primary-image: 1", "provider-ids: 0", "Status: COMPLETE",
		"Home Jellyfin", "10.11.0", "server-1", "https://jellyfin.example", "2026-10-31T12:34:56Z",
		"Image verification: Enabled", "Northern Light · Season 2 · S02E03 · Arrival",
		"Northern Light · Season 2 · S02E04 · After the Storm", "Northern Light · S00 · Specials",
		"  Metadata [overview]: Overview is missing", "  Operational error: Image verification returned HTTP 503", "  Operational error: Timeout verifying backdrop image",
	} {
		if !strings.Contains(terminal.String(), want) {
			t.Errorf("terminal missing %q", want)
		}
	}
	for _, name := range []string{"Healthy movie", "Healthy show", "Healthy documentary"} {
		if strings.Contains(terminal.String(), name) {
			t.Errorf("terminal included healthy row %q", name)
		}
	}
}

func TestTerminalDetailsUseCompactPerItemBlocks(t *testing.T) {
	r := representativeReport()
	longFinding := strings.Repeat("Full overview details remain readable. ", 8) + "End of overview."
	longError := strings.Repeat("Image service connection failed. ", 8) + "End of error."
	r.Results[0].Findings[0].Message = longFinding
	r.Results[0].Errors = []string{longError, "Retry limit reached"}
	r.Results[3].Item.Name = "A Different Show"
	r.Results = []audit.Result{r.Results[0], r.Results[4], r.Results[3], r.Results[1]}
	var b bytes.Buffer
	if err := WriteTerminal(&b, r); err != nil {
		t.Fatal(err)
	}
	_, details, ok := strings.Cut(b.String(), "\nAffected items:\n")
	if !ok {
		t.Fatal("affected items section missing")
	}
	want := "\nThe Quiet City (2024) [Movie] | Library: Movies | ID: movie-1\n" +
		"  Jellyfin: " + r.Results[0].URL + "\n" +
		"  Metadata [overview]: " + longFinding + "\n" +
		"  Metadata [genres]: Genres are missing\n" +
		"  Operational error: " + longError + "\n" +
		"  Operational error: Retry limit reached\n" +
		"\nNorthern Light · Season 2 · S02E04 · After the Storm [Episode] | Library: Television | ID: episode-error\n" +
		"  Operational error: Timeout verifying backdrop image\n" +
		"\nA Different Show [Series] | Library: Television | ID: series-1\n" +
		"  Metadata [overview]: Series overview is missing\n"
	if details != want {
		t.Errorf("terminal detail blocks:\ngot:  %q\nwant: %q", details, want)
	}
	for _, title := range []string{"The Quiet City (2024)", "Northern Light · Season 2 · S02E04 · After the Storm", "A Different Show"} {
		if got := strings.Count(details, title); got != 1 {
			t.Errorf("title %q appears %d times, want once", title, got)
		}
	}
	if strings.Contains(details, "Healthy movie") {
		t.Error("healthy item appeared in detail blocks")
	}
}

func TestCallerRowOrderAndStableGroupStats(t *testing.T) {
	r := representativeReport()
	original := slices.Clone(r.Results)
	v := makeView(r)
	wantIDs := []string{"movie-1", "episode-1", "series-1", "episode-error", "season-0"}
	ids := func(v view) []string {
		var result []string
		for _, row := range v.Rows {
			result = append(result, row.ID)
		}
		return result
	}
	if !slices.Equal(ids(v), wantIDs) {
		t.Errorf("rows = %v, want caller order %v", ids(v), wantIDs)
	}
	if !reflect.DeepEqual(r.Results, original) {
		t.Fatal("reporter mutated caller results")
	}
	slices.Reverse(r.Results)
	reversed := makeView(r)
	if !reflect.DeepEqual(v.Libraries, reversed.Libraries) || !reflect.DeepEqual(v.Types, reversed.Types) || !reflect.DeepEqual(v.Checks, reversed.Checks) {
		t.Fatal("group statistics changed with caller order")
	}
	slices.Reverse(wantIDs)
	if !slices.Equal(ids(reversed), wantIDs) {
		t.Errorf("reversed rows = %v, want %v", ids(reversed), wantIDs)
	}
	var terminal bytes.Buffer
	if err := WriteTerminal(&terminal, r); err != nil {
		t.Fatal(err)
	}
	assertOrder(t, terminal.String(), wantIDs)
}

func assertOrder(t *testing.T, output string, values []string) {
	t.Helper()
	position := -1
	for _, value := range values {
		next := strings.Index(output, value)
		if next <= position {
			t.Fatalf("%q missing or out of order in output", value)
		}
		position = next
	}
}

func TestLibrariesWithIdenticalNamesStaySeparate(t *testing.T) {
	r := audit.Report{Results: []audit.Result{
		{Library: jellyfin.Library{ID: "b", Name: "Movies"}, Findings: []audit.Finding{{Check: "custom", Message: "Missing"}}},
		{Library: jellyfin.Library{ID: "a", Name: "Movies"}},
	}}
	v := makeView(r)
	if len(v.Libraries) != 2 || v.Libraries[0].ID != "a" || v.Libraries[1].ID != "b" || v.Libraries[0].Affected != 0 || v.Libraries[1].Affected != 1 {
		t.Fatalf("same-named library stats merged or unstable: %#v", v.Libraries)
	}
	if !reflect.DeepEqual(v.Checks, []checkCount{{Name: "custom", Count: 1}}) {
		t.Errorf("finding check omitted when absent from Report.Checks: %#v", v.Checks)
	}
}

func TestDisplayTitle(t *testing.T) {
	for _, tt := range []struct {
		name string
		item jellyfin.Item
		want string
	}{
		{"movie year", jellyfin.Item{Type: "Movie", Name: "Film", ProductionYear: new(2020)}, "Film (2020)"},
		{"series", jellyfin.Item{Type: "Series", Name: "Show"}, "Show"},
		{"episode", jellyfin.Item{Type: "Episode", Name: "Pilot", SeriesName: "Show", SeasonName: "Season 1", ParentIndexNumber: new(1), IndexNumber: new(2)}, "Show · Season 1 · S01E02 · Pilot"},
		{"special", jellyfin.Item{Type: "Episode", Name: "Special", SeriesName: "Show", ParentIndexNumber: new(0), IndexNumber: new(0)}, "Show · S00E00 · Special"},
		{"episode without season", jellyfin.Item{Type: "Episode", Name: "Pilot", SeriesName: "Show", IndexNumber: new(1)}, "Show · E01 · Pilot"},
		{"episode without numbers", jellyfin.Item{Type: "Episode", Name: "Pilot", SeriesName: "Show", SeasonName: "Specials"}, "Show · Specials · Pilot"},
		{"season", jellyfin.Item{Type: "Season", Name: "Specials", SeasonName: "Specials", SeriesName: "Show", IndexNumber: new(0)}, "Show · S00 · Specials"},
		{"unnamed", jellyfin.Item{}, "Untitled item"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := DisplayTitle(tt.item); got != tt.want {
				t.Errorf("DisplayTitle = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIncompleteAndEmptyReports(t *testing.T) {
	for _, withResults := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "partial"}[withResults], func(t *testing.T) {
			r := representativeReport()
			r.Complete, r.VerifyImages = false, false
			if !withResults {
				r.Results, r.GeneratedAt = nil, time.Time{}
			}
			var b bytes.Buffer
			if err := WriteTerminal(&b, r); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(b.String(), "!!! INCOMPLETE AUDIT") || !strings.Contains(b.String(), "scanned items only") || strings.Contains(b.String(), "Status: COMPLETE") || !strings.Contains(b.String(), "Disabled (metadata checks only)") {
				t.Error("missing terminal partial-audit or verification status")
			}
			if !withResults && (!strings.Contains(b.String(), "No findings or operational verification errors in scanned items.") || !strings.Contains(b.String(), "Not recorded") || !strings.Contains(b.String(), "Scanned: 0 | Healthy: 0 | Affected: 0")) {
				t.Error("empty report totals or explanation missing")
			}
		})
	}
	t.Run("all healthy", func(t *testing.T) {
		r := audit.Report{Complete: true, Results: []audit.Result{{Item: jellyfin.Item{Name: "Healthy item", Type: "Movie"}}}}
		v := makeView(r)
		if v.Healthy != 1 || v.Affected != 0 || len(v.Rows) != 0 {
			t.Fatalf("healthy report incorrectly classified: %#v", v)
		}
		var b bytes.Buffer
		if err := WriteTerminal(&b, r); err != nil {
			t.Fatal(err)
		}
		output := b.String()
		if strings.Contains(output, "INCOMPLETE AUDIT") || strings.Contains(output, "Healthy item") || !strings.Contains(output, "No findings or operational verification errors in scanned items.") {
			t.Error("healthy report has wrong detail rows or status")
		}
	})
}

func TestTerminalTextStripsControlsAndEscapeSequences(t *testing.T) {
	for _, tt := range []struct{ name, input, want string }{
		{"color", "before\x1b[31mred\x1b[0mafter", "beforeredafter"},
		{"erase", "before\x1b[2Jafter", "beforeafter"},
		{"OSC hyperlink", "\x1b]8;;https://evil.example\x1b\\link\x1b]8;;\x1b\\", "link"},
		{"OSC clipboard", "before\x1b]52;c;c2VjcmV0\aafter", "beforeafter"},
		{"C1", "before\u009b31mred\u009b0mafter", "beforeredafter"},
		{"C1 string", "before\u009d52;c;payload\u009cafter", "beforeafter"},
		{"DCS", "before\x1bPpayload\x1b\\after", "beforeafter"},
		{"single escape", "before\x1b7after", "beforeafter"},
		{"intermediate escape", "before\x1b(Bafter", "beforeafter"},
		{"truncated escape", "before\x1b[31", "before"},
		{"truncated OSC", "before\x1b]52;payload", "before"},
		{"row injection", "one\ntwo\rthree\tfour", "one two three four"},
		{"controls", "a\a\b\x00\x7fb\u0085c", "abc"},
		{"bidi", "normal\u202eevil\u2066text\u2069", "normaleviltext"},
		{"Unicode", "日本語 · Café 🎬", "日本語 · Café 🎬"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := TerminalText(tt.input); got != tt.want {
				t.Errorf("TerminalText(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestTerminalSanitizesEveryUntrustedField(t *testing.T) {
	const poison = "value\x1b[2J\x1b]52;c;payload\a\n\r\t\u009b31m\u202eEND"
	r := audit.Report{
		Server: jellyfin.ServerInfo{ID: poison, ServerName: poison, Version: poison}, ServerURL: poison, Checks: []string{poison},
		Results: []audit.Result{{Library: jellyfin.Library{ID: poison, Name: poison}, Item: jellyfin.Item{ID: poison, Name: poison, Type: poison}, Findings: []audit.Finding{{Check: poison, Message: poison}}, Errors: []string{poison}}},
	}
	var b bytes.Buffer
	if err := WriteTerminal(&b, r); err != nil {
		t.Fatal(err)
	}
	for _, char := range b.String() {
		if (unicode.IsControl(char) && char != '\n') || unicode.Is(unicode.Cf, char) {
			t.Errorf("terminal output contains control %U", char)
		}
	}
	for _, bad := range []string{"payload", "[2J", "[31m", poison} {
		if strings.Contains(b.String(), bad) {
			t.Errorf("terminal output retained %q", bad)
		}
	}
	if strings.Count(b.String(), "value   END") < 10 {
		t.Error("sanitized metadata disappeared from terminal")
	}
}

func TestWriteLibraries(t *testing.T) {
	var b bytes.Buffer
	libs := []jellyfin.Library{
		{ID: "tv", Name: "Shows\x1b[31m red\x1b[0m\nnew", CollectionType: "tvshows\tchild"},
		{ID: "movies\a", Name: "Films", CollectionType: "movies"},
	}
	if err := WriteLibraries(&b, libs); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"LIBRARY ID", "NAME", "COLLECTION TYPE", "tv", "Shows red new", "tvshows child", "movies", "Films"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("library output missing %q", want)
		}
	}
	if strings.ContainsAny(b.String(), "\x1b\a\t\r") {
		t.Errorf("unsanitized library output: %q", b.String())
	}
	assertOrder(t, b.String(), []string{"Shows", "Films"})
	b.Reset()
	if err := WriteLibraries(&b, nil); err != nil || !strings.Contains(b.String(), "No libraries found.") {
		t.Errorf("empty libraries: %q, %v", b.String(), err)
	}
}

var errWrite = errors.New("injected write failure")

// failingWriter allows a real partial render before failing, including when
// backed by an actual temporary report file.
type failingWriter struct {
	dest      io.Writer
	remaining int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	n := min(len(p), w.remaining)
	written, err := w.dest.Write(p[:n])
	w.remaining -= written
	if err != nil {
		return written, err
	}
	if n < len(p) {
		return written, errWrite
	}
	return written, nil
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return max(0, len(p)-1), nil }

func TestWriterErrors(t *testing.T) {
	for _, tt := range []struct {
		name  string
		write func(io.Writer) error
	}{
		{"terminal", func(w io.Writer) error { return WriteTerminal(w, representativeReport()) }},
		{"libraries", func(w io.Writer) error { return WriteLibraries(w, []jellyfin.Library{{ID: "a", Name: "Movies"}}) }},
		{"JSON", func(w io.Writer) error { return WriteJSON(w, representativeReport()) }},
		{"libraries JSON", func(w io.Writer) error { return WriteLibrariesJSON(w, jellyfin.ServerInfo{}, "", nil) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, allowance := range []int{0, 12} {
				writer := &failingWriter{dest: io.Discard, remaining: allowance}
				if err := tt.write(writer); !errors.Is(err, errWrite) {
					t.Errorf("write allowing %d bytes: got %v, want write error", allowance, err)
				}
			}
		})
	}
	for _, write := range []func(io.Writer) error{
		func(w io.Writer) error { return WriteTerminal(w, representativeReport()) },
		func(w io.Writer) error { return WriteLibraries(w, nil) },
		func(w io.Writer) error { return WriteJSON(w, representativeReport()) },
		func(w io.Writer) error { return WriteLibrariesJSON(w, jellyfin.ServerInfo{}, "", nil) },
	} {
		if err := write(shortWriter{}); !errors.Is(err, io.ErrShortWrite) {
			t.Errorf("short write = %v, want io.ErrShortWrite", err)
		}
	}
}

func TestWriteJSONAtomicallyReplacesReport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	if err := os.WriteFile(path, []byte("old report"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := representativeReport()
	if err := WriteJSONFile(path, r); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != renderJSON(t, r) || !json.Valid(got) {
		t.Error("destination did not contain the entire JSON report")
	}
	assertDirectoryEntries(t, dir, []string{"report.json"})
	t.Run("new report", func(t *testing.T) {
		path := filepath.Join(dir, "new report.json")
		if err := WriteJSONFile(path, audit.Report{}); err != nil {
			t.Fatal(err)
		}
		assertDirectoryEntries(t, dir, []string{"new report.json", "report.json"})
	})
}

func TestFailedJSONWritePreservesOldReportAndCleansTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	const old = "previous complete report; must survive a failed write"
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeAtomic(path, func(w io.Writer) error {
		return WriteJSON(&failingWriter{dest: w, remaining: 100}, representativeReport())
	})
	if !errors.Is(err, errWrite) {
		t.Fatalf("failed JSON write = %v, want write error", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != old {
		t.Fatalf("old report changed after failed render: %q, %v", got, err)
	}
	assertDirectoryEntries(t, dir, []string{"report.json"})
}

func TestJSONRenameAndCreateFailures(t *testing.T) {
	t.Run("rename failure", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "report.json")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		keep := filepath.Join(path, "keep")
		if err := os.WriteFile(keep, []byte("preserve me"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := WriteJSONFile(path, representativeReport()); err == nil {
			t.Fatal("expected failure replacing a nonempty directory")
		}
		got, err := os.ReadFile(keep)
		if err != nil || string(got) != "preserve me" {
			t.Error("rename failure damaged destination")
		}
		assertDirectoryEntries(t, dir, []string{"report.json"})
	})
	t.Run("create failure", func(t *testing.T) {
		dir := t.TempDir()
		if err := WriteJSONFile(filepath.Join(dir, "missing", "report.json"), representativeReport()); err == nil {
			t.Fatal("expected failure in missing destination directory")
		}
		assertDirectoryEntries(t, dir, nil)
	})
}

func assertDirectoryEntries(t *testing.T, dir string, want []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	if !slices.Equal(got, want) {
		t.Errorf("directory entries = %v, want %v (temporary files must be removed)", got, want)
	}
}
