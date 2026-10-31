// Package audit checks metadata completeness independently of the Jellyfin API.
package audit

import (
	"time"

	"github.com/Primexz/jellyscout/internal/jellyfin"
)

type Finding struct {
	Check   string
	Message string
}

// Result retains each scanned item so report totals include healthy items.
type Result struct {
	Library  jellyfin.Library
	Item     jellyfin.Item
	URL      string
	Findings []Finding
	Errors   []string // Operational verification failures, not metadata findings.
}

type Report struct {
	Server       jellyfin.ServerInfo
	ServerURL    string
	GeneratedAt  time.Time
	VerifyImages bool
	Complete     bool
	Checks       []string
	Results      []Result
}
