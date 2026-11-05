// Package report renders JellyScout audits without contacting the server.
package report

import (
	"bytes"
	"cmp"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/Primexz/jellyscout/internal/audit"
	"github.com/Primexz/jellyscout/internal/jellyfin"
)

type group struct {
	Name, ID          string
	Scanned, Affected int
}

type checkCount struct {
	Name  string
	Count int
}

type row struct {
	Title, Library, Type, ID, URL string
	Findings                      []audit.Finding
	Errors                        []string
}

type view struct {
	Server                                       jellyfin.ServerInfo
	ServerURL, GeneratedAt, Verification         string
	Complete                                     bool
	Scanned, Healthy, Affected, Findings, Errors int
	Libraries, Types                             []group
	Checks                                       []checkCount
	Rows                                         []row
}

func libraryKey(lib jellyfin.Library) string {
	return fmt.Sprintf("%d:%s%s", len(lib.ID), lib.ID, lib.Name)
}

func makeView(r audit.Report) view {
	v := view{
		Server: r.Server, ServerURL: r.ServerURL, Complete: r.Complete,
		GeneratedAt: "Not recorded", Verification: "Disabled (metadata checks only)",
		Scanned: len(r.Results),
	}
	if !r.GeneratedAt.IsZero() {
		v.GeneratedAt = r.GeneratedAt.Format(time.RFC3339)
	}
	if r.VerifyImages {
		v.Verification = "Enabled (image accessibility checked; failures listed separately)"
	}
	libraries, types, checks := map[string]group{}, map[string]group{}, map[string]int{}
	for _, check := range r.Checks {
		checks[check] = 0
	}
	for _, result := range r.Results {
		affected := len(result.Findings) > 0 || len(result.Errors) > 0
		libKey := libraryKey(result.Library)
		lib := libraries[libKey]
		lib.Name, lib.ID = cmp.Or(result.Library.Name, "Unnamed library"), result.Library.ID
		lib.Scanned++
		typename := cmp.Or(result.Item.Type, "Unknown")
		typ := types[typename]
		typ.Name = typename
		typ.Scanned++
		if affected {
			v.Affected++
			lib.Affected++
			typ.Affected++
			v.Rows = append(v.Rows, row{
				Title: DisplayTitle(result.Item), Library: lib.Name,
				Type: typename, ID: result.Item.ID,
				URL: SafeURL(result.URL), Findings: result.Findings, Errors: result.Errors,
			})
		}
		libraries[libKey], types[typename] = lib, typ
		v.Findings += len(result.Findings)
		v.Errors += len(result.Errors)
		for _, finding := range result.Findings {
			checks[finding.Check]++
		}
	}
	v.Healthy = v.Scanned - v.Affected
	for _, lib := range libraries {
		v.Libraries = append(v.Libraries, lib)
	}
	for _, typ := range types {
		v.Types = append(v.Types, typ)
	}
	groupOrder := func(a, b group) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	}
	slices.SortFunc(v.Libraries, groupOrder)
	slices.SortFunc(v.Types, groupOrder)
	for name, count := range checks {
		v.Checks = append(v.Checks, checkCount{Name: name, Count: count})
	}
	slices.SortFunc(v.Checks, func(a, b checkCount) int { return strings.Compare(a.Name, b.Name) })
	return v
}

// DisplayTitle includes year or television numbering where available.
func DisplayTitle(item jellyfin.Item) string {
	parts := []string{}
	if item.Type == "Episode" || item.Type == "Season" {
		if item.SeriesName != "" {
			parts = append(parts, item.SeriesName)
		}
		if item.SeasonName != "" && item.SeasonName != item.Name {
			parts = append(parts, item.SeasonName)
		}
		context := ""
		if item.Type == "Episode" {
			if item.ParentIndexNumber != nil {
				context = fmt.Sprintf("S%02d", *item.ParentIndexNumber)
			}
			if item.IndexNumber != nil {
				context += fmt.Sprintf("E%02d", *item.IndexNumber)
			}
		} else if item.IndexNumber != nil {
			context = fmt.Sprintf("S%02d", *item.IndexNumber)
		}
		if context != "" {
			parts = append(parts, context)
		}
	}
	name := cmp.Or(item.Name, "Untitled item")
	if item.Type == "Movie" && item.ProductionYear != nil {
		name += fmt.Sprintf(" (%d)", *item.ProductionYear)
	}
	return strings.Join(append(parts, name), " · ")
}

// SafeURL allows only absolute HTTP(S) links without credentials or terminal controls.
func SafeURL(raw string) string {
	if strings.ContainsRune(raw, '\\') || strings.ContainsFunc(raw, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r) || unicode.Is(unicode.Cf, r)
	}) {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" || u.Opaque != "" ||
		(!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) {
		return ""
	}
	// Jellyfin may also accept API credentials in query parameters, including
	// the web client's fragment query. Do not export links containing them.
	_, fragmentQuery, _ := strings.Cut(u.Fragment, "?")
	for _, query := range []string{u.RawQuery, fragmentQuery} {
		params, err := url.ParseQuery(query)
		if err != nil {
			return ""
		}
		for key := range params {
			switch strings.ToLower(key) {
			case "api_key", "apikey", "api-key", "access_token", "token", "x-emby-token", "x-mediabrowser-token":
				return ""
			}
		}
	}
	return raw
}

// WriteTerminal writes a plain-text audit, including totals for healthy items.
// Every value originating from the server is stripped of terminal controls.
func WriteTerminal(w io.Writer, r audit.Report) error {
	v := makeView(r)
	var b bytes.Buffer
	fmt.Fprintln(&b, "JellyScout · Metadata audit")
	if !v.Complete {
		fmt.Fprintln(&b, "!!! INCOMPLETE AUDIT — totals cover scanned items only; do not infer library health !!!")
	} else {
		fmt.Fprintln(&b, "Status: COMPLETE")
	}
	fmt.Fprintf(&b, "Server: %s | version %s | ID %s\nJellyfin URL: %s\nGenerated: %s\nImage verification: %s\n\n",
		TerminalText(v.Server.ServerName), TerminalText(v.Server.Version), TerminalText(v.Server.ID),
		SafeURL(v.ServerURL), v.GeneratedAt, v.Verification)
	fmt.Fprintf(&b, "Scanned: %d | Healthy: %d | Affected: %d\nMetadata findings: %d | Operational verification errors: %d\n",
		v.Scanned, v.Healthy, v.Affected, v.Findings, v.Errors)
	fmt.Fprintln(&b, "\nFindings by check:")
	for _, check := range v.Checks {
		fmt.Fprintf(&b, "  %s: %d\n", TerminalText(check.Name), check.Count)
	}
	if len(v.Checks) == 0 {
		fmt.Fprintln(&b, "  No checks recorded")
	}
	for _, section := range []struct {
		name   string
		groups []group
	}{{"Library", v.Libraries}, {"Type", v.Types}} {
		fmt.Fprintf(&b, "\n%s totals (scanned / affected):\n", section.name)
		for _, g := range section.groups {
			label := TerminalText(g.Name)
			if g.ID != "" {
				label += " [" + TerminalText(g.ID) + "]"
			}
			fmt.Fprintf(&b, "  %s: %d / %d\n", label, g.Scanned, g.Affected)
		}
	}
	fmt.Fprintln(&b, "\nAffected items:")
	for _, item := range v.Rows {
		fmt.Fprintf(&b, "\n%s [%s] | Library: %s | ID: %s\n", TerminalText(item.Title),
			TerminalText(item.Type), TerminalText(item.Library), TerminalText(item.ID))
		if item.URL != "" {
			fmt.Fprintf(&b, "  Jellyfin: %s\n", item.URL)
		}
		for _, finding := range item.Findings {
			fmt.Fprintf(&b, "  Metadata [%s]: %s\n", TerminalText(finding.Check), TerminalText(finding.Message))
		}
		for _, message := range item.Errors {
			fmt.Fprintf(&b, "  Operational error: %s\n", TerminalText(message))
		}
	}
	if len(v.Rows) == 0 {
		fmt.Fprintln(&b, "No findings or operational verification errors in scanned items.")
	}
	_, err := b.WriteTo(w)
	return err
}

// WriteLibraries lists library identifiers for use in selecting an audit scope.
// Input order is preserved.
func WriteLibraries(w io.Writer, libs []jellyfin.Library) error {
	var b bytes.Buffer
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	// The tabwriter targets an in-memory buffer, so its writes cannot fail.
	_, _ = fmt.Fprintln(tw, "LIBRARY ID\tNAME\tCOLLECTION TYPE")
	for _, lib := range libs {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", TerminalText(lib.ID), TerminalText(lib.Name), TerminalText(lib.CollectionType))
	}
	_ = tw.Flush()
	if len(libs) == 0 {
		fmt.Fprintln(&b, "No libraries found.")
	}
	_, err := b.WriteTo(w)
	return err
}

// TerminalText removes ANSI/VT escape sequences, C0/C1 controls and Unicode
// format controls (including bidi overrides). Tabs and line breaks in metadata
// become spaces so untrusted text cannot create rows or columns.
func TerminalText(s string) string {
	var b strings.Builder
	state := "text"
	for _, r := range s {
		switch state {
		case "escape":
			switch r {
			case '[':
				state = "csi"
			case ']', 'P', 'X', '^', '_':
				state = "string"
			default:
				if r < 0x20 || r > 0x2f {
					state = "text"
				}
			}
		case "csi":
			if r >= 0x40 && r <= 0x7e {
				state = "text"
			}
		case "string":
			switch r {
			case '\a', 0x9c:
				state = "text"
			case 0x1b:
				state = "string escape"
			}
		case "string escape":
			if r == '\\' || r == 0x9c || r == '\a' {
				state = "text"
			} else if r != 0x1b {
				state = "string"
			}
		default:
			switch r {
			case 0x1b:
				state = "escape"
			case 0x9b:
				state = "csi"
			case 0x90, 0x98, 0x9d, 0x9e, 0x9f:
				state = "string"
			case '\n', '\r', '\t':
				b.WriteByte(' ')
			default:
				if !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) {
					b.WriteRune(r)
				}
			}
		}
	}
	return b.String()
}
