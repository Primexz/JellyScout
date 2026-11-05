<h1 align="center">JellyScout</h1>

<p align="center">
  Find missing artwork and incomplete metadata in your Jellyfin library.
</p>

<p align="center">
  <a href="#install"><img src="https://img.shields.io/badge/Go-1.26%2B-00ADD8?style=for-the-badge&amp;logo=go&amp;logoColor=white&amp;labelColor=1e293b" alt="Go 1.26 or newer"></a>
  <a href="https://github.com/Primexz/JellyScout/actions/workflows/ci.yml?query=branch%3Amaster"><img src="https://img.shields.io/github/actions/workflow/status/Primexz/JellyScout/ci.yml?branch=master&amp;style=for-the-badge&amp;label=CI&amp;labelColor=1e293b" alt="CI workflow status on master"></a>
  <a href="#output-and-exit-codes"><img src="https://img.shields.io/badge/Output-Terminal%20%2F%20JSON-0D9488?style=for-the-badge&amp;labelColor=1e293b" alt="Terminal and JSON output"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-2563EB?style=for-the-badge&amp;labelColor=1e293b" alt="MIT license"></a>
</p>

## Features

- Audit movies and shows, with optional season and episode checks.
- Filter by library and check type; optionally fetch and decode artwork.
- View terminal totals and affected items, including the server URL and direct Jellyfin item links.
- Export JSON for scripts and use exit codes for automated checks.
- Review findings interactively and confirm provider refreshes for selected items.


## Install

Requires **Go 1.26+**:

```sh
go install github.com/Primexz/jellyscout/cmd/jellyscout@latest
```

Put Go's binary directory on your `PATH`: `GOBIN` if set, otherwise `$(go env GOPATH)/bin`.
To build from a local checkout instead:

```sh
go build -o bin/ ./cmd/jellyscout
./bin/jellyscout --help
```

## Quick start

Create a server API key in the Jellyfin dashboard, then set your connection details:

```sh
export JELLYFIN_URL="http://localhost:8096"
export JELLYFIN_API_KEY="your-api-key"

jellyscout libraries
jellyscout scan
```

Use the server root URL, including a reverse-proxy base path such as `https://media.example.com/jellyfin`, without `/web` or `/api-docs`.
Supply a server API key; username/password and user session authentication are unsupported.
`--url` and `--api-key` override the environment. Redirects are not followed, so use the final URL.
API keys are excluded from reports and item links.

## Checks

All applicable checks are enabled by default. Blank or whitespace-only text counts as missing.

| Check | Movies and shows | Seasons | Episodes |
| --- | --- | --- | --- |
| `primary-image` | Own poster | Own poster | Own primary still |
| `backdrop` | Own backdrop | — | — |
| `title` | Title | Title | Title |
| `overview` | Synopsis | — | Synopsis |
| `year` | Positive production year | — | — |
| `genres` | At least one genre | — | — |
| `provider-id` | IMDb, TMDb, or TVDb ID | — | — |
| `numbering` | — | Season number | Season and episode numbers |

Parent or series artwork never satisfies an item's own artwork requirement. Numbering must be nonnegative; zero is valid. Provider names are case-insensitive.

## CLI usage

Flags follow the command. Scans include movies and shows in all libraries by default. Use `libraries` to find IDs, then repeat `--library` to select libraries:

```sh
jellyscout scan --library MOVIE_LIBRARY_ID --library TV_LIBRARY_ID
jellyscout scan --include-seasons --include-episodes
jellyscout scan --include-episodes --checks primary-image,overview,numbering
jellyscout scan --verify-images --concurrency 2 --timeout 60s
jellyscout scan --help
```

`--include-seasons` and `--include-episodes` work independently. Missing/virtual episodes
and placeholder videos are skipped. `--checks` replaces the default check selection;
only checks applicable to each item type run. Empty or unknown names are rejected.

By default, artwork checks inspect API references only. `--verify-images` fetches small
renditions of the referenced primary image and every backdrop covered by enabled checks,
then decodes them. Defaults are four concurrent image requests and a 30-second timeout
per HTTP request. HTTP 404 and invalid image data are findings; timeouts and server
errors mark verification incomplete. Authentication failures and HTTP 429 rate limits
stop the scan.

## Interactive autofix

Run the interactive view in a terminal with the same connection and scan flags:

```sh
jellyscout tui
jellyscout tui --library TV_LIBRARY_ID --include-seasons --include-episodes
jellyscout tui --verify-images --repair-timeout 2m --json final-report.json
```

Both stdin and stdout must be terminals. `tui` accepts the `scan` flags except
`--format`, plus `--repair-timeout` (a positive duration, default `2m`). Progress
appears inside the interface while it is active. `--json` saves the latest report
using the existing JSON schema when you quit normally; an interrupted or failed
session preserves any existing report file. `--fail-on-issues` applies to the
findings remaining in that final report.

| Key | Action |
| --- | --- |
| `j` / `k`, down / up arrows | Move between items or scroll an overlay |
| Space | Toggle selection of the current eligible item |
| `a` | Toggle selection of all visible eligible items |
| `/` | Search by title, item ID, or library |
| `l` / `t` / `c` / `s` | Cycle library / item type / check / status filters |
| Enter | Open item details |
| `r` | Review the selected repair batch |
| Ctrl+R | Rescan the configured libraries and checks |
| Esc | Close an overlay or cancel the active local batch |
| `q` | Quit |

The review defaults to **Cancel**. Press Tab to switch the action and Enter to
activate it, or press `y` to confirm explicitly. Review every action's scope:
refreshing a show or season affects its subtree, including descendants hidden by
the current filters or excluded from the audit. Selected descendants covered by
a parent action are grouped into that action. Selection and search filters do
not restrict Jellyfin's recursive refresh scope.

Repairs ask Jellyfin's configured providers to fill missing metadata and artwork,
respecting metadata locks. There is no manual metadata editing. Existing metadata
and images are not deliberately replaced, so corrupt referenced images can remain
unresolved. Provider availability and configuration determine what Jellyfin can find.

HTTP `204` means the refresh was **queued**, not completed. JellyScout rechecks
items for up to two minutes per action by default; `--repair-timeout` changes that
wait. The interface distinguishes resolved, partially resolved, and unresolved
findings. Authentication failures or rate limits stop further dispatch. Canceling
local work does not cancel refresh jobs Jellyfin has already accepted; timed-out
or unresolved items may still change later. Use Ctrl+R to inspect their latest state.

## Output and exit codes

Terminal output is the default. Use JSON on stdout or save an additional JSON file:

```sh
jellyscout scan --format json
jellyscout libraries --format json
jellyscout scan --json report.json
jellyscout scan --fail-on-issues --json report.json
```

Progress and errors go to stderr. JSON includes `schema_version: 1`, server details and URL,
selected checks, summary totals, and an `items` array containing affected items only,
with item URLs, findings, and operational errors. Totals include healthy items.
Partial image-verification reports have `complete: false` and exit with code `2`.

`--json report.json` writes an additional file atomically, preserving an existing file on
scan or write failures. Incomplete image-verification reports are saved.
Shell redirection has the shell's usual file-truncation behavior.

| Code | Meaning |
| --- | --- |
| `0` | Completed successfully; findings are allowed by default |
| `1` | Completed with findings and `--fail-on-issues` enabled |
| `2` | Invalid arguments, operational failure, or incomplete verification |

## Development

CI runs golangci-lint and tests Ubuntu, macOS, and Windows using the Go version from `go.mod`.
Install the pinned linter version locally:

```sh
curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b ./bin "$(cat .golangci-lint-version)"
```

Run these checks from the repository root; tests use simulated servers without credentials:

```sh
test -z "$(gofmt -l cmd internal)"
go mod tidy -diff
./bin/golangci-lint run
go vet ./...
go test -race -coverprofile=coverage.out ./...
go build -o bin/ ./cmd/jellyscout
```

## License

JellyScout is [MIT licensed](LICENSE). Copyright © 2026 Primexz.
