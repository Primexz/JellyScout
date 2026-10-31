package audit

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Primexz/jellyscout/internal/jellyfin"
)

func TestDefaultChecks(t *testing.T) {
	want := []string{"primary-image", "backdrop", "title", "overview", "year", "genres", "provider-id", "numbering"}
	if got := DefaultChecks(); !slices.Equal(got, want) {
		t.Fatalf("DefaultChecks() = %v, want %v", got, want)
	}
	checks := DefaultChecks()
	checks[0] = "modified"
	if got := DefaultChecks(); !slices.Equal(got, want) {
		t.Fatalf("caller mutation changed defaults: %v", got)
	}
}

func TestParseChecks(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value string
		want  []string
	}{
		{"single", "title", []string{"title"}},
		{"canonical order", "numbering,provider-id,genres,year,overview,title,backdrop,primary-image", DefaultChecks()},
		{"trim and deduplicate", " numbering, title ,primary-image,title, numbering ", []string{"primary-image", "title", "numbering"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseChecks(tt.value)
			if err != nil || !slices.Equal(got, tt.want) {
				t.Fatalf("ParseChecks(%q) = %v, %v; want %v, nil", tt.value, got, err, tt.want)
			}
		})
	}
	for _, value := range []string{"", " \t\n", ",title", "title,", "title,,year", "title, \t,year", "unknown", "title,unknown", "Title"} {
		t.Run("reject "+value, func(t *testing.T) {
			got, err := ParseChecks(value)
			if err == nil || len(got) != 0 {
				t.Fatalf("ParseChecks(%q) = %v, %v; want no checks and an error", value, got, err)
			}
		})
	}
}

func TestCheckMissingAndHealthyByType(t *testing.T) {
	for _, tt := range []struct {
		itemType string
		missing  []string
	}{
		{"Movie", []string{"primary-image", "backdrop", "title", "overview", "year", "genres", "provider-id"}},
		{"Series", []string{"primary-image", "backdrop", "title", "overview", "year", "genres", "provider-id"}},
		{"Season", []string{"primary-image", "title", "numbering"}},
		{"Episode", []string{"primary-image", "title", "overview", "numbering"}},
		{"Audio", nil},
		{"Folder", nil},
		{"BoxSet", nil},
		{"movie", nil},
		{"", nil},
	} {
		t.Run(tt.itemType, func(t *testing.T) {
			assertChecks(t, Check(jellyfin.Item{Type: tt.itemType}, DefaultChecks()), tt.missing)
			assertChecks(t, Check(healthyItem(tt.itemType), DefaultChecks()), nil)
		})
	}
}

func TestCheckPresenceTrimsWhitespace(t *testing.T) {
	for _, itemType := range []string{"Movie", "Series", "Season", "Episode"} {
		t.Run(itemType, func(t *testing.T) {
			item := healthyItem(itemType)
			item.Name = " \t\n\u2003"
			item.Overview = "\t\n "
			item.ImageTags = map[string]string{"Primary": " \n\t"}
			item.BackdropImageTags = []string{"", " \t", "\n"}
			item.Genres = []string{"", " \t", "\n"}
			item.ProviderIDs = map[string]string{"IMDb": " \t", "TMDb": "", "TVDb": "\n"}
			want := []string{"primary-image", "backdrop", "title", "overview", "genres", "provider-id"}
			switch itemType {
			case "Season":
				want = []string{"primary-image", "title"}
			case "Episode":
				want = []string{"primary-image", "title", "overview"}
			}
			assertChecks(t, Check(item, DefaultChecks()), want)
		})
	}
	item := healthyItem("Movie")
	item.Name = " Title \n"
	item.Overview = " Overview \t"
	item.ImageTags["Primary"] = " primary \t"
	item.BackdropImageTags = []string{" ", " backdrop ", ""}
	item.Genres = []string{" ", " Drama ", ""}
	item.ProviderIDs = map[string]string{"IMDb": " tt123 "}
	assertChecks(t, Check(item, DefaultChecks()), nil)
}

func TestCheckProductionYear(t *testing.T) {
	for _, itemType := range []string{"Movie", "Series"} {
		for _, tt := range []struct {
			name string
			year *int
			want []string
		}{
			{"null", nil, []string{"year"}},
			{"zero", new(0), []string{"year"}},
			{"negative", new(-1), []string{"year"}},
			{"positive", new(1), nil},
		} {
			t.Run(itemType+"/"+tt.name, func(t *testing.T) {
				item := healthyItem(itemType)
				item.ProductionYear = tt.year
				assertChecks(t, Check(item, DefaultChecks()), tt.want)
			})
		}
	}
}

func TestCheckNumbering(t *testing.T) {
	for _, tt := range []struct {
		name     string
		itemType string
		index    *int
		parent   *int
		want     []string
	}{
		{"season null", "Season", nil, new(1), []string{"numbering"}},
		{"season negative", "Season", new(-1), new(1), []string{"numbering"}},
		{"season zero", "Season", new(0), nil, nil},
		{"season positive", "Season", new(2), new(-1), nil},
		{"episode null index", "Episode", nil, new(0), []string{"numbering"}},
		{"episode null parent", "Episode", new(0), nil, []string{"numbering"}},
		{"episode both null", "Episode", nil, nil, []string{"numbering"}},
		{"episode negative index", "Episode", new(-1), new(0), []string{"numbering"}},
		{"episode negative parent", "Episode", new(0), new(-1), []string{"numbering"}},
		{"episode both negative", "Episode", new(-1), new(-1), []string{"numbering"}},
		{"episode zeros", "Episode", new(0), new(0), nil},
		{"episode positive", "Episode", new(3), new(2), nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			item := healthyItem(tt.itemType)
			item.IndexNumber, item.ParentIndexNumber = tt.index, tt.parent
			assertChecks(t, Check(item, DefaultChecks()), tt.want)
		})
	}
}

func TestCheckProviderIDs(t *testing.T) {
	for _, itemType := range []string{"Movie", "Series"} {
		for _, tt := range []struct {
			name string
			ids  map[string]string
			want []string
		}{
			{"null", nil, []string{"provider-id"}},
			{"unknown", map[string]string{"MusicBrainz": "123", "custom": "456"}, []string{"provider-id"}},
			{"blank recognized with unknown", map[string]string{"IMDb": " \n", "custom": "123"}, []string{"provider-id"}},
			{"IMDb mixed case", map[string]string{"iMdB": " tt123 "}, nil},
			{"TMDb uppercase", map[string]string{"TMDB": "123"}, nil},
			{"TVDb lowercase", map[string]string{"tvdb": "123"}, nil},
			{"one nonblank", map[string]string{"IMDb": "", "TMDb": " \t", "TVDb": "456"}, nil},
		} {
			t.Run(itemType+"/"+tt.name, func(t *testing.T) {
				item := healthyItem(itemType)
				item.ProviderIDs = tt.ids
				assertChecks(t, Check(item, DefaultChecks()), tt.want)
			})
		}
	}
}

func TestCheckEnabledFilters(t *testing.T) {
	for _, tt := range []struct {
		name     string
		itemType string
		checks   []string
		want     []string
	}{
		{"nil", "Movie", nil, nil},
		{"empty", "Movie", []string{}, nil},
		{"subset ordered and deduplicated", "Movie", []string{"year", "title", "year"}, []string{"title", "year"}},
		{"unknown ignored", "Movie", []string{"unknown", "TITLE"}, nil},
		{"movie numbering inapplicable", "Movie", []string{"numbering"}, nil},
		{"series numbering inapplicable", "Series", []string{"numbering"}, nil},
		{"season metadata inapplicable", "Season", []string{"backdrop", "overview", "year", "genres", "provider-id"}, nil},
		{"episode metadata inapplicable", "Episode", []string{"backdrop", "year", "genres", "provider-id"}, nil},
		{"episode subset", "Episode", []string{"overview", "numbering"}, []string{"overview", "numbering"}},
		{"unsupported", "Audio", DefaultChecks(), nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertChecks(t, Check(jellyfin.Item{Type: tt.itemType}, tt.checks), tt.want)
		})
	}
}

func TestCheckIgnoresIrrelevantFieldsAndInheritedContext(t *testing.T) {
	for _, itemType := range []string{"Movie", "Series", "Season", "Episode"} {
		t.Run(itemType, func(t *testing.T) {
			item := healthyItem(itemType)
			item.Name, item.Overview = "", ""
			item.ImageTags = map[string]string{"Thumb": "thumb", "Backdrop": "backdrop", "Logo": "logo"}
			item.BackdropImageTags = nil
			item.ParentID, item.SeriesID, item.SeasonID = "parent", "series", "season"
			item.SeriesName, item.SeasonName = "Series title", "Season title"
			item.Path, item.CollectionType, item.LocationType = "/Title (2024)", "movies", "Virtual"
			item.IsPlaceHolder = true
			want := []string{"primary-image", "backdrop", "title", "overview"}
			switch itemType {
			case "Season":
				want = []string{"primary-image", "title"}
			case "Episode":
				want = []string{"primary-image", "title", "overview"}
			}
			assertChecks(t, Check(item, DefaultChecks()), want)
		})
	}
}

func TestImageRefsOwnImagesAndOriginalIndices(t *testing.T) {
	for _, itemType := range []string{"Movie", "Series", "Season", "Episode", "Audio"} {
		t.Run(itemType, func(t *testing.T) {
			item := jellyfin.Item{
				ID: "item-id", Type: itemType,
				ImageTags:         map[string]string{"Primary": " primary ", "Thumb": "thumb"},
				BackdropImageTags: []string{"", " backdrop-one ", " \t", "backdrop-three", "backdrop-three"},
				ParentID:          "parent", SeriesID: "series", SeasonID: "season",
			}
			var want []jellyfin.ImageRef
			if itemType != "Audio" {
				want = []jellyfin.ImageRef{{ItemID: "item-id", Type: "Primary", Index: 0, Tag: " primary "}}
			}
			if itemType == "Movie" || itemType == "Series" {
				want = append(want,
					jellyfin.ImageRef{ItemID: "item-id", Type: "Backdrop", Index: 1, Tag: " backdrop-one "},
					jellyfin.ImageRef{ItemID: "item-id", Type: "Backdrop", Index: 3, Tag: "backdrop-three"},
					jellyfin.ImageRef{ItemID: "item-id", Type: "Backdrop", Index: 4, Tag: "backdrop-three"},
				)
			}
			if got := ImageRefs(item, DefaultChecks()); !reflect.DeepEqual(got, want) {
				t.Fatalf("ImageRefs() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestImageRefsFiltersAndBlankTags(t *testing.T) {
	for _, tt := range []struct {
		name      string
		checks    []string
		primary   string
		backdrops []string
		want      []jellyfin.ImageRef
	}{
		{"nil checks", nil, "primary", []string{"backdrop"}, nil},
		{"empty checks", []string{}, "primary", []string{"backdrop"}, nil},
		{"unrelated checks", []string{"title", "unknown"}, "primary", []string{"backdrop"}, nil},
		{"primary only", []string{"primary-image", "primary-image"}, "primary", []string{"backdrop"}, []jellyfin.ImageRef{{ItemID: "id", Type: "Primary", Tag: "primary"}}},
		{"backdrop only", []string{"backdrop", "backdrop"}, "primary", []string{"", "backdrop"}, []jellyfin.ImageRef{{ItemID: "id", Type: "Backdrop", Index: 1, Tag: "backdrop"}}},
		{"all blank", DefaultChecks(), " \t\n", []string{"", " \t", "\n"}, nil},
		{"missing", DefaultChecks(), "", nil, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			item := jellyfin.Item{
				ID: "id", Type: "Movie", ImageTags: map[string]string{"Primary": tt.primary, "Thumb": "thumb", "Backdrop": "inherited"},
				BackdropImageTags: tt.backdrops, ParentID: "parent",
			}
			if got := ImageRefs(item, tt.checks); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ImageRefs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestSortResults(t *testing.T) {
	// Each adjacent pair differs first at one of the documented sort keys.
	want := []Result{
		{Library: jellyfin.Library{Name: "A", ID: "1"}, Item: jellyfin.Item{Type: "Episode", Name: "A", ID: "1"}, URL: "first tie"},
		{Library: jellyfin.Library{Name: "A", ID: "1"}, Item: jellyfin.Item{Type: "Episode", Name: "A", ID: "1"}, URL: "second tie"},
		{Library: jellyfin.Library{Name: "A", ID: "1"}, Item: jellyfin.Item{Type: "Episode", Name: "A", ID: "2"}},
		{Library: jellyfin.Library{Name: "A", ID: "1"}, Item: jellyfin.Item{Type: "Episode", Name: "B", ID: "0"}},
		{Library: jellyfin.Library{Name: "A", ID: "1"}, Item: jellyfin.Item{Type: "Movie", Name: "A", ID: "0"}},
		{Library: jellyfin.Library{Name: "A", ID: "2"}, Item: jellyfin.Item{Type: "Episode", Name: "A", ID: "0"}},
		{Library: jellyfin.Library{Name: "B", ID: "0"}, Item: jellyfin.Item{Type: "Episode", Name: "A", ID: "0"}},
	}
	for _, order := range [][]int{{6, 4, 3, 0, 5, 2, 1}, {2, 5, 0, 4, 1, 6, 3}} {
		report := Report{Complete: true, VerifyImages: true, Checks: []string{"title"}, GeneratedAt: time.Unix(123, 0), ServerURL: "https://example.test", Server: jellyfin.ServerInfo{ID: "server"}}
		for _, i := range order {
			report.Results = append(report.Results, want[i])
		}
		expected := report
		expected.Results = want
		Sort(&report)
		if !reflect.DeepEqual(report, expected) {
			t.Fatalf("Sort() = %#v, want %#v", report, expected)
		}
		Sort(&report)
		if !reflect.DeepEqual(report, expected) {
			t.Fatal("Sort is not idempotent")
		}
	}
}

func TestSortFindingsAndErrors(t *testing.T) {
	wantFindings := []Finding{
		{Check: "primary-image", Message: "primary"},
		{Check: "backdrop", Message: "backdrop"},
		{Check: "title", Message: "a"},
		{Check: "title", Message: "a"},
		{Check: "title", Message: "z"},
		{Check: "overview", Message: "overview"},
		{Check: "year", Message: "year"},
		{Check: "genres", Message: "genres"},
		{Check: "provider-id", Message: "provider"},
		{Check: "numbering", Message: "numbering"},
		{Check: "custom-a", Message: "a"},
		{Check: "custom-z", Message: "z"},
	}
	for _, order := range [][]int{{11, 9, 7, 5, 3, 1, 10, 8, 6, 4, 2, 0}, {9, 0, 11, 2, 8, 5, 3, 10, 4, 1, 6, 7}} {
		report := Report{Results: []Result{{Errors: []string{" z ", "a", "", "a", "Z"}}, {}}}
		for _, i := range order {
			report.Results[0].Findings = append(report.Results[0].Findings, wantFindings[i])
		}
		Sort(&report)
		if got := report.Results[0].Findings; !reflect.DeepEqual(got, wantFindings) {
			t.Fatalf("sorted findings = %#v, want %#v", got, wantFindings)
		}
		if got, want := report.Results[0].Errors, []string{"", " z ", "Z", "a", "a"}; !slices.Equal(got, want) {
			t.Fatalf("sorted errors = %q, want %q (including blanks and duplicates)", got, want)
		}
		if report.Results[1].Findings != nil || report.Results[1].Errors != nil {
			t.Fatal("sorting empty result changed nil slices")
		}
	}
	Sort(&Report{})
}

func healthyItem(itemType string) jellyfin.Item {
	return jellyfin.Item{
		ID: "item", Type: itemType, Name: "Title", Overview: "Overview",
		ProductionYear: new(2024), Genres: []string{"Drama"}, ProviderIDs: map[string]string{"IMDb": "tt123"},
		ImageTags: map[string]string{"Primary": "primary"}, BackdropImageTags: []string{"backdrop"},
		IndexNumber: new(1), ParentIndexNumber: new(1),
	}
}

func assertChecks(t *testing.T, findings []Finding, want []string) {
	t.Helper()
	var got []string
	for _, finding := range findings {
		got = append(got, finding.Check)
		if strings.TrimSpace(finding.Message) == "" || finding.Message == finding.Check {
			t.Errorf("finding %q must have a human-readable message: %q", finding.Check, finding.Message)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("finding checks = %v, want %v", got, want)
	}
}
