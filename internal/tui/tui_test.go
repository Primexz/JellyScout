package tui

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Primexz/jellyscout/internal/audit"
	"github.com/Primexz/jellyscout/internal/jellyfin"
	"github.com/Primexz/jellyscout/internal/repair"
	"github.com/Primexz/jellyscout/internal/scan"
)

type fakeRepair struct {
	plans, runs atomic.Int32
	wait        bool
}

func (f *fakeRepair) Plan(_ context.Context, results []audit.Result) (repair.Plan, error) {
	f.plans.Add(1)
	return repair.Plan{Actions: []repair.Action{{Result: results[0], Metadata: true}}, Observed: results}, nil
}

func (f *fakeRepair) Run(ctx context.Context, plan repair.Plan, emit func(repair.Event)) error {
	f.runs.Add(1)
	emit(repair.Event{ID: plan.Actions[0].Result.Item.ID, State: "queued", Message: "Accepted by Jellyfin."})
	if f.wait {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func readyModel(t *testing.T) (*model, *fakeRepair) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	client, err := jellyfin.New("http://localhost:8096/jf", "test-key", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(ctx, client, Options{Scan: scan.Options{Checks: []string{"overview"}, Concurrency: 2}})
	f := &fakeRepair{}
	m.engine = f
	m.loaded = true
	m.report = audit.Report{Complete: true, ServerURL: client.BaseURL(), Checks: []string{"overview"}, Results: []audit.Result{
		{Item: jellyfin.Item{ID: "a", Name: "Alpha", Type: "Movie"}, Library: jellyfin.Library{ID: "movies", Name: "Movies"}, URL: client.ItemURL("a", "server"), Findings: []audit.Finding{{Check: "overview", Message: "Missing overview."}}},
		{Item: jellyfin.Item{ID: "b", Name: "Beta", Type: "Series"}, Library: jellyfin.Library{ID: "tv", Name: "TV"}, URL: client.ItemURL("b", "server"), Findings: []audit.Finding{{Check: "overview", Message: "Missing overview."}}},
	}}
	t.Cleanup(func() { cancel(); m.work.Wait(); client.Close() })
	return m, f
}

func press(m *model, code rune) { m.Update(tea.KeyPressMsg{Code: code}) }
func applyNext(t *testing.T, m *model) any {
	t.Helper()
	select {
	case e := <-m.events:
		m.Update(e)
		return e.value
	case <-time.After(2 * time.Second):
		t.Fatal("no worker message")
		return nil
	}
}

func TestReviewDefaultsToCancelAndRequiresApproval(t *testing.T) {
	m, f := readyModel(t)
	press(m, tea.KeySpace)
	press(m, 'r')
	applyNext(t, m)
	if m.overlay != "review" || m.confirm || f.runs.Load() != 0 {
		t.Fatal("review must not submit requests")
	}
	press(m, tea.KeyEnter)
	if m.overlay != "" || f.runs.Load() != 0 {
		t.Fatal("Enter on default Cancel submitted a request")
	}
	press(m, 'r')
	applyNext(t, m)
	press(m, tea.KeyTab)
	press(m, tea.KeyEnter)
	applyNext(t, m)
	if f.runs.Load() != 1 || m.state(m.report.Results[0]) != "queued" || len(m.report.Results[0].Findings) != 1 {
		t.Fatal("queued request must retain unresolved audit findings")
	}
	applyNext(t, m)
	if m.busy != "" || f.plans.Load() != 2 {
		t.Fatal("batch did not finish")
	}
}

func TestSelectionUsesIDsAcrossFiltersAndSearch(t *testing.T) {
	m, _ := readyModel(t)
	press(m, tea.KeySpace)
	press(m, '/')
	m.Update(tea.KeyPressMsg{Text: "Beta", Code: 'B'})
	press(m, tea.KeyEnter)
	press(m, tea.KeySpace)
	if !m.selected["a"] || !m.selected["b"] || len(m.selected) != 2 {
		t.Fatal("filter changed selected identities")
	}
	press(m, 'a')
	if !m.selected["a"] || m.selected["b"] {
		t.Fatal("toggle visible affected hidden selection")
	}
	press(m, tea.KeyEscape)
	press(m, 'l')
	if rows := m.rows(); len(rows) != 1 || rows[0].Item.ID != "a" {
		t.Fatalf("library filter: %+v", rows)
	}
	press(m, tea.KeyEscape)
	press(m, 't')
	if len(m.rows()) != 1 {
		t.Fatal("type filter failed")
	}
	press(m, tea.KeyEscape)
	press(m, 'c')
	if len(m.rows()) != 2 {
		t.Fatal("check filter failed")
	}
}

func TestCancelStopsLocalBatch(t *testing.T) {
	m, f := readyModel(t)
	f.wait = true
	press(m, tea.KeySpace)
	press(m, 'r')
	applyNext(t, m)
	press(m, 'y')
	applyNext(t, m)
	press(m, tea.KeyEscape)
	applyNext(t, m)
	if m.busy != "" || m.err == nil || !strings.Contains(m.notice, "may continue") {
		t.Fatal("cancellation must explain accepted server jobs")
	}
}

func TestStaleMessagesAndObservedResults(t *testing.T) {
	m, _ := readyModel(t)
	m.generation = 3
	m.Update(envelope{generation: 2, value: scanDone{report: audit.Report{}}})
	if len(m.report.Results) != 2 {
		t.Fatal("stale scan replaced current report")
	}
	fresh := m.report.Results[0]
	fresh.Findings = nil
	m.Update(envelope{generation: 3, value: repair.Event{ID: "a", State: "resolved", Results: []audit.Result{fresh}}})
	if len(m.report.Results[0].Findings) != 0 || m.state(fresh) != "resolved" {
		t.Fatal("fresh audit was not merged")
	}
	if len(m.rows()) != 2 {
		t.Fatal("repair outcomes should remain visible during this session")
	}
}

func TestViewURLsSanitizationResizeAndScope(t *testing.T) {
	m, _ := readyModel(t)
	m.report.Results[0].Item.Name = "Film\x1b]0;INJECTED\a\nTitle\x1b[31m"
	v := m.View()
	plain := ansi.Strip(v.Content)
	if !v.AltScreen || !strings.Contains(plain, "http://localhost:8096/jf") || !strings.Contains(plain, "web/index.html") || strings.Contains(plain, "INJECTED") || !strings.Contains(plain, "Film Title") {
		t.Fatalf("unsafe or missing terminal content:\n%s", plain)
	}
	m.plan = repair.Plan{Actions: []repair.Action{{Result: m.report.Results[1], Metadata: true, Descendants: []jellyfin.Item{{ID: "child", Type: "Episode", Name: "Outside scan"}}}}}
	m.overlay = "review"
	text := strings.Join(m.reviewLines(), "\n")
	for _, want := range []string{"ENTIRE subtree", "outside audit filters", "Outside scan", "metadata", "queues a refresh"} {
		if !strings.Contains(text, want) {
			t.Errorf("review missing %q", want)
		}
	}
	for _, size := range [][2]int{{1, 1}, {40, 10}, {52, 14}, {90, 25}, {120, 40}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		view := m.View().Content
		if len(strings.Split(view, "\n")) > size[1] {
			t.Fatal("view exceeds terminal height")
		}
		for _, line := range strings.Split(view, "\n") {
			if ansi.StringWidth(line) > size[0] {
				t.Fatal("view exceeds terminal width")
			}
		}
	}
}

func TestNonTerminalRunDoesNotContactServer(t *testing.T) {
	if IsTerminal(strings.NewReader(""), &strings.Builder{}) {
		t.Fatal("pipes detected as terminal")
	}
	_, err := Run(t.Context(), strings.NewReader(""), &strings.Builder{}, nil, Options{})
	if err == nil || !strings.Contains(err.Error(), "interactive") {
		t.Fatal("non-terminal Run must fail before using client")
	}
}

func TestSuccessfulReviewClearsOldErrorAndHealthySelections(t *testing.T) {
	m, _ := readyModel(t)
	m.selected["a"] = true
	m.receive(planDone{err: errors.New("temporary read failure")})
	fresh := m.report.Results[0]
	fresh.Findings = nil
	m.receive(planDone{plan: repair.Plan{Observed: []audit.Result{fresh}}})
	if m.err != nil || m.selected["a"] || len(m.selected) != 0 {
		t.Fatal("successful review must clear stale errors and newly healthy selections")
	}
	if m.report.Results[0].Item.ID != "a" || !m.report.Complete {
		t.Fatal("fresh audit was not retained")
	}
}

func TestTransientUnknownCanResolveWithoutError(t *testing.T) {
	m, _ := readyModel(t)
	m.receive(repair.Event{ID: "a", State: "unknown", Message: "POST connection closed"})
	fresh := m.report.Results[0]
	fresh.Findings = nil
	m.receive(repair.Event{ID: "a", State: "resolved", Results: []audit.Result{fresh}})
	m.receive(batchDone{})
	if m.err != nil || m.state(fresh) != "resolved" {
		t.Fatal("verified resolution retained transient submission error")
	}
}

func TestCoveredDescendantsHaveTheirOwnObservedStatus(t *testing.T) {
	m, _ := readyModel(t)
	fresh := m.report.Results[0]
	fresh.Findings = nil
	m.receive(repair.Event{ID: "b", State: "partially resolved", Results: []audit.Result{fresh, m.report.Results[1]}})
	if m.state(m.report.Results[0]) != "resolved" || m.state(m.report.Results[1]) != "partially resolved" {
		t.Fatal("descendant status did not follow its own findings")
	}
}

func TestCannotApproveReviewWhenTerminalIsTooSmall(t *testing.T) {
	m, f := readyModel(t)
	press(m, tea.KeySpace)
	press(m, 'r')
	applyNext(t, m)
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	press(m, 'y')
	press(m, tea.KeyTab)
	press(m, tea.KeyEnter)
	if f.runs.Load() != 0 || m.confirm {
		t.Fatal("approved a batch while review was hidden by the resize warning")
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	press(m, tea.KeyEnter)
	if f.runs.Load() != 0 || m.overlay != "" {
		t.Fatal("restored review must still default to Cancel")
	}
}
