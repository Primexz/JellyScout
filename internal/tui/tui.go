// Package tui provides the interactive audit and provider-refresh workflow.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/term"

	"github.com/Primexz/jellyscout/internal/audit"
	"github.com/Primexz/jellyscout/internal/jellyfin"
	"github.com/Primexz/jellyscout/internal/repair"
	"github.com/Primexz/jellyscout/internal/report"
	"github.com/Primexz/jellyscout/internal/scan"
)

type Options struct {
	Scan          scan.Options
	RepairTimeout time.Duration
}

// IsTerminal checks the supplied streams, without opening a separate terminal.
func IsTerminal(stdin io.Reader, stdout io.Writer) bool {
	in, okIn := stdin.(interface{ Fd() uintptr })
	out, okOut := stdout.(interface{ Fd() uintptr })
	return okIn && okOut && term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

// Run returns the latest observed audit after restoring the terminal. Refresh
// requests already accepted by Jellyfin continue when local work is canceled.
func Run(ctx context.Context, stdin io.Reader, stdout io.Writer, client *jellyfin.Client, opts Options) (audit.Report, error) {
	if !IsTerminal(stdin, stdout) {
		return audit.Report{}, errors.New("tui requires interactive stdin and stdout; use jellyscout scan for terminal or JSON reports")
	}
	ctx, cancel := context.WithCancel(ctx)
	m := newModel(ctx, client, opts)
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(stdin), tea.WithOutput(stdout))
	_, err := p.Run()
	cancel()
	m.work.Wait()
	if err != nil {
		return m.report, fmt.Errorf("interactive session: %w", err)
	}
	return m.report, m.err
}

type repairer interface {
	Plan(context.Context, []audit.Result) (repair.Plan, error)
	Run(context.Context, repair.Plan, func(repair.Event)) error
}

type envelope struct {
	generation int
	value      any
}
type progressMsg string
type scanDone struct {
	report audit.Report
	err    error
}
type planDone struct {
	plan repair.Plan
	err  error
}
type batchDone struct{ err error }

type model struct {
	ctx                          context.Context
	client                       *jellyfin.Client
	opts                         Options
	engine                       repairer
	work                         sync.WaitGroup
	events                       chan envelope
	cancel                       context.CancelFunc
	generation                   int
	report                       audit.Report
	loaded                       bool
	err                          error
	busy                         string
	notice                       string
	selected                     map[string]bool
	statuses                     map[string]repair.Event
	cursor                       int
	width, height                int
	query                        string
	searching                    bool
	library, kind, check, status string
	overlay                      string
	scroll                       int
	plan                         repair.Plan
	confirm                      bool
}

func newModel(ctx context.Context, client *jellyfin.Client, opts Options) *model {
	if opts.RepairTimeout <= 0 {
		opts.RepairTimeout = 2 * time.Minute
	}
	return &model{
		ctx: ctx, client: client, opts: opts,
		engine: repair.New(client, repair.Options{Checks: opts.Scan.Checks,
			VerifyImages: opts.Scan.VerifyImages, Concurrency: opts.Scan.Concurrency, Timeout: opts.RepairTimeout}),
		events: make(chan envelope, 64), selected: make(map[string]bool), statuses: make(map[string]repair.Event),
		width: 100, height: 30,
	}
}

func (m *model) Init() tea.Cmd {
	m.startScan()
	return m.listen()
}

func (m *model) listen() tea.Cmd {
	return func() tea.Msg {
		select {
		case e := <-m.events:
			return e
		case <-m.ctx.Done():
			return nil
		}
	}
}

func (m *model) operation(name string) (context.Context, func(any)) {
	if m.cancel != nil {
		m.cancel()
	}
	m.generation++
	generation := m.generation
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel, m.busy = cancel, name
	return ctx, func(value any) {
		select {
		case m.events <- envelope{generation: generation, value: value}:
		case <-m.ctx.Done():
		}
	}
}

func (m *model) startScan() {
	ctx, emit := m.operation("scanning")
	client, opts := m.client, m.opts.Scan
	m.notice = "Loading Jellyfin libraries…"
	m.work.Go(func() {
		r, err := scan.Run(ctx, client, opts, func(s string) { emit(progressMsg(s)) })
		emit(scanDone{r, err})
	})
}

func (m *model) startPlan() {
	var selected []audit.Result
	for _, result := range m.report.Results {
		if m.selected[result.Item.ID] {
			selected = append(selected, result)
		}
	}
	if len(selected) == 0 {
		m.notice = "Select items with Space, then press r to review a batch."
		return
	}
	ctx, emit := m.operation("preparing review")
	m.notice = "Rechecking selected items, locks, and descendant scope…"
	engine := m.engine
	m.work.Go(func() {
		plan, err := engine.Plan(ctx, selected)
		emit(planDone{plan, err})
	})
}

func (m *model) startBatch() {
	plan, engine := m.plan, m.engine
	m.overlay, m.confirm = "", false
	ctx, emit := m.operation("repairing")
	m.notice = "Submitting provider refreshes; accepted server jobs continue if you cancel."
	for _, result := range plan.Observed {
		delete(m.selected, result.Item.ID)
	}
	m.work.Go(func() {
		err := engine.Run(ctx, plan, func(e repair.Event) { emit(e) })
		emit(batchDone{err})
	})
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, msg.Width), max(1, msg.Height)
	case envelope:
		if msg.generation == m.generation {
			m.receive(msg.value)
		}
		return m, m.listen()
	case tea.KeyPressMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m *model) receive(value any) {
	switch value := value.(type) {
	case progressMsg:
		m.notice = report.TerminalText(string(value))
	case scanDone:
		m.busy = ""
		if value.err != nil {
			m.err = value.err
			m.notice = "Scan failed: " + value.err.Error()
			return
		}
		m.report, m.loaded, m.err = value.report, true, nil
		clear(m.statuses)
		valid := make(map[string]bool)
		for _, r := range m.report.Results {
			if len(r.Findings) > 0 {
				valid[r.Item.ID] = true
			}
		}
		for id := range m.selected {
			if !valid[id] {
				delete(m.selected, id)
			}
		}
		m.notice = "Audit ready. Select affected items, then press r to review repairs."
		m.cursor = 0
	case planDone:
		m.busy = ""
		if value.err != nil {
			m.err = value.err
			m.notice = "Could not prepare repair batch: " + value.err.Error()
			return
		}
		m.merge(value.plan.Observed)
		m.err = nil
		for _, skipped := range value.plan.Skipped {
			m.statuses[skipped.ID] = skipped
		}
		m.plan, m.overlay, m.scroll, m.confirm = value.plan, "review", 0, false
		m.notice = "Review the provider actions and their scope before confirming."
	case repair.Event:
		m.statuses[value.ID] = value
		m.merge(value.Results)
		for _, result := range value.Results {
			if result.Item.ID == value.ID {
				continue
			}
			child := value
			child.ID, child.Results = result.Item.ID, nil
			if value.State != "queued" && value.State != "skipped" && value.State != "failed" && value.State != "unknown" && len(result.Findings) == 0 && len(result.Errors) == 0 {
				child.State, child.Message = "resolved", "Findings disappeared in a fresh audit."
			} else if value.State == "partially resolved" && len(result.Findings) > 0 {
				child.State = "unresolved"
			}
			m.statuses[child.ID] = child
		}
		m.notice = value.State + ": " + value.Message
	case batchDone:
		m.busy = ""
		m.err = value.err
		if value.err != nil {
			m.err = value.err
			m.notice = "Local repair work stopped: " + value.err.Error() + ". Accepted Jellyfin jobs may continue."
		} else {
			m.notice = "Batch recheck finished. Unresolved items may still be refreshing on Jellyfin."
		}
	}
}

func (m *model) merge(results []audit.Result) {
	indices := make(map[string]int, len(m.report.Results))
	for i, result := range m.report.Results {
		indices[result.Item.ID] = i
	}
	for _, result := range results {
		if i, ok := indices[result.Item.ID]; ok {
			m.report.Results[i] = result
		}
		if len(result.Findings) == 0 {
			delete(m.selected, result.Item.ID)
		}
	}
	if len(results) > 0 {
		m.report.GeneratedAt = time.Now().UTC()
		m.report.Complete = !slices.ContainsFunc(m.report.Results, func(r audit.Result) bool { return len(r.Errors) > 0 })
	}
}

func (m *model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	tooSmall := m.width < 52 || m.height < 14
	if key == "ctrl+c" || (key == "q" && (!m.searching || tooSmall)) {
		if m.busy != "" {
			m.err = errors.New("local work canceled; accepted Jellyfin refreshes may continue")
		}
		if !m.loaded && m.err == nil {
			m.err = errors.New("audit was not completed")
		}
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit
	}
	if tooSmall {
		if key == "esc" {
			m.overlay, m.searching, m.confirm = "", false, false
			if m.busy != "" && m.cancel != nil {
				m.cancel()
				m.notice = "Canceling local work. Already accepted Jellyfin refreshes continue."
			}
		}
		return m, nil
	}
	if m.searching {
		switch key {
		case "enter":
			m.searching = false
		case "esc":
			m.searching, m.query = false, ""
		case "backspace":
			runes := []rune(m.query)
			m.query = string(runes[:max(0, len(runes)-1)])
		default:
			if msg.Text != "" {
				m.query += report.TerminalText(msg.Text)
			}
		}
		m.cursor = 0
		return m, nil
	}
	if m.overlay != "" {
		return m.overlayKey(key)
	}
	if key == "esc" && m.busy != "" {
		m.cancel()
		m.notice = "Canceling local work. Already accepted Jellyfin refreshes continue."
		return m, nil
	}
	rows := m.rows()
	switch key {
	case "down", "j":
		m.cursor = min(max(0, len(rows)-1), m.cursor+1)
	case "up", "k":
		m.cursor = max(0, m.cursor-1)
	case "home":
		m.cursor = 0
	case "end":
		m.cursor = max(0, len(rows)-1)
	case "pgdown":
		m.cursor = min(max(0, len(rows)-1), m.cursor+max(1, m.height-12))
	case "pgup":
		m.cursor = max(0, m.cursor-max(1, m.height-12))
	case "space", " ":
		if m.busy == "" && len(rows) > 0 && len(rows[m.cursor].Findings) > 0 {
			id := rows[m.cursor].Item.ID
			if m.selected[id] {
				delete(m.selected, id)
			} else {
				m.selected[id] = true
			}
		}
	case "a":
		if m.busy == "" {
			all := true
			for _, r := range rows {
				if len(r.Findings) > 0 && !m.selected[r.Item.ID] {
					all = false
				}
			}
			for _, r := range rows {
				if len(r.Findings) > 0 {
					if all {
						delete(m.selected, r.Item.ID)
					} else {
						m.selected[r.Item.ID] = true
					}
				}
			}
		}
	case "enter":
		if len(rows) > 0 {
			m.overlay, m.scroll = "details", 0
		}
	case "/":
		m.searching = true
	case "l", "t", "c", "s":
		m.cycleFilter(key)
		m.cursor = 0
	case "r":
		if m.busy == "" && m.loaded {
			m.startPlan()
		}
	case "ctrl+r":
		if m.busy == "" {
			m.startScan()
		}
	case "esc":
		m.query, m.library, m.kind, m.check, m.status = "", "", "", "", ""
		m.cursor = 0
	}
	return m, nil
}

func (m *model) overlayKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc", "n":
		m.overlay, m.confirm = "", false
	case "up", "k":
		m.scroll = max(0, m.scroll-1)
	case "down", "j":
		m.scroll++
	case "pgup":
		m.scroll = max(0, m.scroll-max(1, m.height-12))
	case "pgdown":
		m.scroll += max(1, m.height-12)
	case "tab", "left", "right":
		if m.overlay == "review" && len(m.plan.Actions) > 0 {
			m.confirm = !m.confirm
		}
	case "enter":
		if m.overlay == "review" && m.confirm && len(m.plan.Actions) > 0 {
			m.startBatch()
		} else {
			m.overlay = ""
		}
	case "y":
		if m.overlay == "review" && len(m.plan.Actions) > 0 {
			m.startBatch()
		}
	}
	return m, nil
}

func (m *model) state(r audit.Result) string {
	if e, ok := m.statuses[r.Item.ID]; ok {
		return e.State
	}
	if len(r.Errors) > 0 {
		return "error"
	}
	if len(r.Findings) > 0 {
		return "findings"
	}
	return "healthy"
}

func (m *model) rows() []audit.Result {
	var rows []audit.Result
	for _, r := range m.report.Results {
		if len(r.Findings) == 0 && len(r.Errors) == 0 && m.statuses[r.Item.ID].State == "" {
			continue
		}
		if m.library != "" && m.library != r.Library.ID {
			continue
		}
		if m.kind != "" && m.kind != r.Item.Type {
			continue
		}
		if m.status != "" && m.status != m.state(r) {
			continue
		}
		if m.check != "" && !slices.ContainsFunc(r.Findings, func(f audit.Finding) bool { return f.Check == m.check }) {
			continue
		}
		haystack := strings.ToLower(report.TerminalText(report.DisplayTitle(r.Item) + " " + r.Item.ID + " " + r.Library.Name))
		if m.query != "" && !strings.Contains(haystack, strings.ToLower(m.query)) {
			continue
		}
		rows = append(rows, r)
	}
	m.cursor = min(m.cursor, max(0, len(rows)-1))
	return rows
}

func cycle(current string, values []string) string {
	return values[(slices.Index(values, current)+1)%len(values)]
}

func (m *model) cycleFilter(key string) {
	switch key {
	case "l":
		values := []string{""}
		for _, r := range m.report.Results {
			if !slices.Contains(values, r.Library.ID) {
				values = append(values, r.Library.ID)
			}
		}
		m.library = cycle(m.library, values)
	case "t":
		m.kind = cycle(m.kind, []string{"", "Movie", "Series", "Season", "Episode"})
	case "c":
		m.check = cycle(m.check, append([]string{""}, m.opts.Scan.Checks...))
	case "s":
		m.status = cycle(m.status, []string{"", "findings", "queued", "checking", "resolved", "partially resolved", "unresolved", "skipped", "failed", "unknown", "error"})
	}
}
