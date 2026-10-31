package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Primexz/jellyscout/internal/audit"
	"github.com/Primexz/jellyscout/internal/jellyfin"
)

// These DTOs deliberately include only report data, never raw API items.
type jsonServer struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	URL     string `json:"url"`
}

type jsonLibrary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type jsonLibraryTotal struct {
	jsonLibrary
	Scanned  int `json:"scanned"`
	Affected int `json:"affected"`
}

type jsonTypeTotal struct {
	Type     string `json:"type"`
	Scanned  int    `json:"scanned"`
	Affected int    `json:"affected"`
}

type jsonCheckTotal struct {
	Check    string `json:"check"`
	Findings int    `json:"findings"`
}

type jsonSummary struct {
	Scanned           int                `json:"scanned"`
	Healthy           int                `json:"healthy"`
	Affected          int                `json:"affected"`
	Findings          int                `json:"findings"`
	OperationalErrors int                `json:"operational_errors"`
	ByLibrary         []jsonLibraryTotal `json:"by_library"`
	ByType            []jsonTypeTotal    `json:"by_type"`
	ByCheck           []jsonCheckTotal   `json:"by_check"`
}

type jsonFinding struct {
	Check   string `json:"check"`
	Message string `json:"message"`
}

type jsonItem struct {
	ID            string        `json:"id"`
	Type          string        `json:"type"`
	Name          string        `json:"name"`
	Title         string        `json:"title"`
	Library       jsonLibrary   `json:"library"`
	URL           string        `json:"url"`
	Path          string        `json:"path"`
	Findings      []jsonFinding `json:"findings"`
	Errors        []string      `json:"errors"`
	SeriesName    string        `json:"series_name,omitempty"`
	SeasonName    string        `json:"season_name,omitempty"`
	SeasonNumber  *int          `json:"season_number,omitzero"`
	EpisodeNumber *int          `json:"episode_number,omitzero"`
}

type jsonReport struct {
	SchemaVersion int         `json:"schema_version"`
	Server        jsonServer  `json:"server"`
	GeneratedAt   time.Time   `json:"generated_at"`
	Complete      bool        `json:"complete"`
	VerifyImages  bool        `json:"verify_images"`
	Checks        []string    `json:"checks"`
	Summary       jsonSummary `json:"summary"`
	Items         []jsonItem  `json:"items"`
}

func makeJSONServer(server jellyfin.ServerInfo, serverURL string) jsonServer {
	return jsonServer{ID: server.ID, Name: server.ServerName, Version: server.Version, URL: safeURL(serverURL)}
}

// WriteJSON writes a versioned audit report. Totals include every scanned item;
// item details include only findings and operational verification failures.
func WriteJSON(w io.Writer, r audit.Report) error {
	v := makeView(r)
	doc := jsonReport{
		SchemaVersion: 1, Server: makeJSONServer(r.Server, r.ServerURL),
		GeneratedAt: r.GeneratedAt, Complete: r.Complete, VerifyImages: r.VerifyImages,
		Checks: append([]string{}, r.Checks...), Items: []jsonItem{},
		Summary: jsonSummary{
			Scanned: v.Scanned, Healthy: v.Healthy, Affected: v.Affected,
			Findings: v.Findings, OperationalErrors: v.Errors,
			ByLibrary: []jsonLibraryTotal{}, ByType: []jsonTypeTotal{}, ByCheck: []jsonCheckTotal{},
		},
	}
	for _, lib := range v.Libraries {
		doc.Summary.ByLibrary = append(doc.Summary.ByLibrary, jsonLibraryTotal{
			jsonLibrary: jsonLibrary{ID: lib.ID, Name: lib.Name}, Scanned: lib.Scanned, Affected: lib.Affected,
		})
	}
	for _, typ := range v.Types {
		doc.Summary.ByType = append(doc.Summary.ByType, jsonTypeTotal{Type: typ.Name, Scanned: typ.Scanned, Affected: typ.Affected})
	}
	for _, check := range v.Checks {
		doc.Summary.ByCheck = append(doc.Summary.ByCheck, jsonCheckTotal{Check: check.Name, Findings: check.Count})
	}
	for _, result := range r.Results {
		if len(result.Findings) == 0 && len(result.Errors) == 0 {
			continue
		}
		item := result.Item
		entry := jsonItem{
			ID: item.ID, Type: item.Type, Name: item.Name, Title: displayTitle(item),
			Library: jsonLibrary{ID: result.Library.ID, Name: result.Library.Name},
			URL:     safeURL(result.URL), Path: item.Path,
			Findings: []jsonFinding{}, Errors: append([]string{}, result.Errors...),
			SeriesName: item.SeriesName, SeasonName: item.SeasonName,
		}
		switch item.Type {
		case "Episode":
			entry.SeasonNumber, entry.EpisodeNumber = item.ParentIndexNumber, item.IndexNumber
		case "Season":
			entry.SeasonNumber = item.IndexNumber
		}
		for _, finding := range result.Findings {
			entry.Findings = append(entry.Findings, jsonFinding{Check: finding.Check, Message: finding.Message})
		}
		doc.Items = append(doc.Items, entry)
	}
	return writeJSON(w, doc)
}

type jsonLibraryEntry struct {
	jsonLibrary
	CollectionType string `json:"collection_type"`
}

// WriteLibrariesJSON lists library identifiers in the caller's order.
func WriteLibrariesJSON(w io.Writer, server jellyfin.ServerInfo, serverURL string, libs []jellyfin.Library) error {
	doc := struct {
		SchemaVersion int                `json:"schema_version"`
		Server        jsonServer         `json:"server"`
		Libraries     []jsonLibraryEntry `json:"libraries"`
	}{SchemaVersion: 1, Server: makeJSONServer(server, serverURL), Libraries: []jsonLibraryEntry{}}
	for _, lib := range libs {
		doc.Libraries = append(doc.Libraries, jsonLibraryEntry{
			jsonLibrary: jsonLibrary{ID: lib.ID, Name: lib.Name}, CollectionType: lib.CollectionType,
		})
	}
	return writeJSON(w, doc)
}

func writeJSON(w io.Writer, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	// Buffer.WriteTo also detects short writes from an io.Writer.
	_, err = bytes.NewBuffer(append(data, '\n')).WriteTo(w)
	return err
}

// WriteJSONFile atomically replaces path after the complete report is written
// and closed. A failed write leaves any existing destination untouched.
func WriteJSONFile(path string, r audit.Report) error {
	return writeAtomic(path, func(w io.Writer) error { return WriteJSON(w, r) })
}

func writeAtomic(path string, render func(io.Writer) error) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".jellyscout-*.json")
	if err != nil {
		return fmt.Errorf("create JSON report: %w", err)
	}
	// Cleanup is best effort: the checked close/rename below governs success.
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	if err := render(f); err != nil {
		return fmt.Errorf("write JSON report: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close JSON report: %w", err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("replace JSON report: %w", err)
	}
	return nil
}
