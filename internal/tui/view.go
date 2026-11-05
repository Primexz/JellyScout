package tui

import (
	"cmp"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Primexz/jellyscout/internal/audit"
	"github.com/Primexz/jellyscout/internal/report"
)

var (
	accent  = lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Bold(true)
	dim     = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	good    = lipgloss.NewStyle().Foreground(lipgloss.Color("114"))
	warning = lipgloss.NewStyle().Foreground(lipgloss.Color("215"))
	focus   = lipgloss.NewStyle().Background(lipgloss.Color("236"))
)

func clean(s string) string              { return report.TerminalText(s) }
func clipped(s string, width int) string { return ansi.Truncate(s, max(0, width), "…") }
func cell(s string, width int) string {
	s = clipped(clean(s), width)
	return s + strings.Repeat(" ", max(0, width-ansi.StringWidth(s)))
}

func (m *model) View() tea.View {
	return m.render()
}

// render composes a bounded screen even while a terminal is being resized.
func (m *model) render() tea.View {
	var lines []string
	phase := cmp.Or(m.busy, "audit")
	lines = append(lines, accent.Render("JellyScout")+dim.Render("  /  "+phase))
	serverURL := m.report.ServerURL
	if serverURL == "" && m.client != nil {
		serverURL = m.client.BaseURL()
	}
	lines = append(lines, "Jellyfin: "+report.SafeURL(serverURL))
	findings, affected, healthy := 0, 0, 0
	for _, result := range m.report.Results {
		findings += len(result.Findings)
		if len(result.Findings)+len(result.Errors) > 0 {
			affected++
		} else {
			healthy++
		}
	}
	lines = append(lines, fmt.Sprintf("%d scanned  ·  %d healthy  ·  %d affected  ·  %d findings  ·  %d selected",
		len(m.report.Results), healthy, affected, findings, len(m.selected)))
	if m.loaded && !m.report.Complete {
		lines = append(lines, warning.Render("INCOMPLETE AUDIT · verification errors are listed in item details"))
	}
	lines = append(lines, dim.Render(strings.Repeat("─", max(1, m.width))))
	footer := []string{dim.Render(clean(m.notice))}
	var content []string
	switch m.overlay {
	case "review":
		content = m.reviewLines()
		buttons := "[ Cancel ]    Refresh batch"
		if m.confirm {
			buttons = "  Cancel    [ Refresh batch ]"
		}
		if len(m.plan.Actions) == 0 {
			buttons = "[ Close · no eligible actions ]"
		}
		footer = append(footer, accent.Render(buttons), dim.Render("Tab choose · Enter activate · y confirm batch · Esc cancel · ↑↓ scroll"))
	case "details":
		content = m.detailLines()
		footer = append(footer, dim.Render("↑↓ / PgUp PgDn scroll · Enter / Esc close · q quit"))
	default:
		library := m.library
		for _, r := range m.report.Results {
			if r.Library.ID == m.library {
				library = r.Library.Name
				break
			}
		}
		filters := fmt.Sprintf("Library [l]: %s  Type [t]: %s  Check [c]: %s  Status [s]: %s",
			cmp.Or(clean(library), "all"), cmp.Or(m.kind, "all"), cmp.Or(m.check, "all"), cmp.Or(m.status, "all"))
		lines = append(lines, dim.Render(filters))
		if m.searching || m.query != "" {
			lines = append(lines, accent.Render("Search: "+clean(m.query)+"▏"))
		}
		rows := m.rows()
		if len(rows) > 0 {
			current := rows[m.cursor]
			footer = append(footer, "Item: "+report.SafeURL(current.URL))
		} else {
			footer = append(footer, dim.Render("No affected items match these filters."))
		}
		footer = append(footer, dim.Render("↑↓ j/k move · Space select · a toggle visible · / search · Enter details"),
			dim.Render("r review repairs · Ctrl+R rescan · Esc clear / stop local work · q quit"))
		available := max(1, m.height-len(lines)-len(footer)-1)
		content = m.tableLines(rows, available)
	}
	available := max(0, m.height-len(lines)-len(footer)-1)
	if m.overlay != "" {
		m.scroll = min(m.scroll, max(0, len(content)-max(1, available)))
		content = content[m.scroll:min(len(content), m.scroll+available)]
	} else if len(content) > available {
		content = content[:available]
	}
	lines = append(lines, content...)
	for len(lines) < m.height-len(footer)-1 {
		lines = append(lines, "")
	}
	lines = append(lines, dim.Render(strings.Repeat("─", max(1, m.width))))
	lines = append(lines, footer...)
	if m.width < 52 || m.height < 14 {
		lines = []string{accent.Render("JellyScout"), "Jellyfin: " + report.SafeURL(serverURL), "Resize terminal to at least 52 × 14.", "q quit · Esc cancel local work"}
	}
	if len(lines) > m.height {
		lines = lines[:m.height]
	}
	for i := range lines {
		lines[i] = clipped(lines[i], m.width)
	}
	v := tea.NewView(strings.Join(lines, "\n"))
	v.AltScreen = true
	return v
}

func (m *model) tableLines(rows []audit.Result, height int) []string {
	if len(rows) == 0 {
		message := "No metadata findings."
		if m.busy != "" {
			message = "Working… " + clean(m.notice)
		} else if !m.loaded {
			message = "No audit available. Ctrl+R retries the scan."
		}
		return []string{message}
	}
	titleWidth := max(12, m.width-46)
	if m.width < 85 {
		titleWidth = max(12, m.width-30)
	}
	wide := m.width >= 85
	row := func(mark, state, kind, title, library string, count string) string {
		s := mark + " " + cell(state, 17) + " " + cell(kind, 7) + " " + cell(title, titleWidth)
		if wide {
			s += " " + cell(library, 12)
		}
		return s + " " + count
	}
	lines := []string{dim.Render(row(" ", "STATUS", "TYPE", "TITLE", "LIBRARY", "ISSUES"))}
	visible := max(1, height-1)
	start := max(0, m.cursor-visible+1)
	for i := start; i < min(len(rows), start+visible); i++ {
		r := rows[i]
		mark := "○"
		if m.selected[r.Item.ID] {
			mark = "●"
		}
		line := row(mark, m.state(r), r.Item.Type, report.DisplayTitle(r.Item), r.Library.Name, fmt.Sprint(len(r.Findings)))
		if i == m.cursor {
			line = focus.Render(line)
		}
		lines = append(lines, line)
	}
	return lines
}

func (m *model) detailLines() []string {
	rows := m.rows()
	if len(rows) == 0 {
		return []string{"No item selected."}
	}
	r := rows[m.cursor]
	lines := []string{accent.Render(clean(report.DisplayTitle(r.Item))), clean(r.Item.Type + " · " + r.Library.Name + " · ID " + r.Item.ID),
		"Jellyfin: " + report.SafeURL(r.URL), "Path: " + clean(r.Item.Path), "Status: " + m.state(r), ""}
	if status, ok := m.statuses[r.Item.ID]; ok {
		lines = append(lines, clean(status.Message), "")
	}
	if len(r.Findings) == 0 {
		lines = append(lines, good.Render("No remaining findings in the observed audit."))
	}
	for _, f := range r.Findings {
		lines = append(lines, warning.Render(clean(f.Check))+": "+clean(f.Message))
	}
	for _, err := range r.Errors {
		lines = append(lines, warning.Render("Verification error: ")+clean(err))
	}
	lines = append(lines, "", "Refreshes use the server's configured metadata and image providers.",
		"Existing corrupt artwork may remain unresolved without replacement.")
	return m.wrap(lines)
}

func (m *model) reviewLines() []string {
	lines := []string{accent.Render("Review provider refresh batch"),
		fmt.Sprintf("%d actions · %d skipped · timeout %s per accepted request", len(m.plan.Actions), len(m.plan.Skipped), m.opts.RepairTimeout),
		"Refreshes may fill other missing fields. No blanket metadata or image replacement.",
		"Series and seasons refresh their ENTIRE subtree, including outside audit filters.",
		"Jellyfin may discover new items or reconcile stale library entries in that scope.", ""}
	for _, action := range m.plan.Actions {
		modes := []string{}
		if action.Metadata {
			modes = append(modes, "metadata")
		}
		if action.Images {
			modes = append(modes, "artwork")
		}
		lines = append(lines, accent.Render(clean(report.DisplayTitle(action.Result.Item))),
			clean(action.Result.Item.Type+" · "+action.Result.Library.Name+" · ID "+action.Result.Item.ID),
			"Refresh: "+strings.Join(modes, " + "), "Jellyfin: "+report.SafeURL(action.Result.URL))
		if action.Result.Item.Type == "Series" || action.Result.Item.Type == "Season" {
			lines = append(lines, warning.Render(fmt.Sprintf("Recursive scope: entire subtree (%d currently known descendants).", len(action.Descendants))))
			for _, child := range action.Descendants[:min(10, len(action.Descendants))] {
				lines = append(lines, "  ↳ "+clean(child.Type+" · "+report.DisplayTitle(child)+" ["+child.ID+"]"))
			}
			if len(action.Descendants) > 10 {
				lines = append(lines, fmt.Sprintf("  … and %d more descendants, all included.", len(action.Descendants)-10))
			}
		}
		lines = append(lines, "")
	}
	for _, skipped := range m.plan.Skipped {
		lines = append(lines, dim.Render("Skipped "+clean(skipped.ID)+": "+clean(skipped.Message)))
	}
	lines = append(lines, "", "HTTP 204 only queues a refresh. Findings are rechecked to establish observed results.",
		"Quitting stops local requests and polling; accepted Jellyfin jobs keep running.")
	return m.wrap(lines)
}

func (m *model) wrap(lines []string) []string {
	var wrapped []string
	for _, line := range lines {
		wrapped = append(wrapped, strings.Split(ansi.Hardwrap(line, max(1, m.width), false), "\n")...)
	}
	return wrapped
}
