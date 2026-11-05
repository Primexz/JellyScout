package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTUIRejectsNonTerminalBeforeHTTP(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "must not be contacted", http.StatusInternalServerError)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, []byte("existing report"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	getenv := func(key string) string {
		switch key {
		case "JELLYFIN_URL":
			return server.URL
		case "JELLYFIN_API_KEY":
			return "cli-secret"
		}
		return ""
	}
	code := RunWithInput(t.Context(), []string{"tui", "--json", path}, strings.NewReader("q"), &stdout, &stderr, getenv)
	if code != 2 || requests.Load() != 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "requires terminal stdin and stdout") {
		t.Fatalf("code=%d requests=%d stdout=%s stderr=%s", code, requests.Load(), &stdout, &stderr)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "existing report" {
		t.Fatalf("report changed: %q, %v", data, err)
	}
}

func TestTUIHelpAndInvalidFlags(t *testing.T) {
	code, out, errout := invoke(t.Context(), "", "tui", "--help")
	if code != 0 {
		t.Fatalf("help code=%d: %s", code, errout)
	}
	for _, want := range []string{"-library", "-checks", "-concurrency", "-verify-images", "-include-seasons", "-include-episodes", "-fail-on-issues", "-json", "-repair-timeout", "default 2m0s"} {
		if !strings.Contains(errout, want) {
			t.Errorf("help missing %q: %s", want, errout)
		}
	}
	if strings.Contains(out+errout, "cli-secret") || strings.Contains(errout, "-format") {
		t.Fatalf("secret or unsupported format in help: %s%s", out, errout)
	}
	for _, tc := range []struct {
		args    []string
		message string
	}{
		{[]string{"--repair-timeout", "0s"}, "--repair-timeout must be positive"},
		{[]string{"--repair-timeout", "-1s"}, "--repair-timeout must be positive"},
		{[]string{"--repair-timeout", "invalid"}, "invalid value"},
		{[]string{"--format", "json"}, "flag provided but not defined"},
		{[]string{"--checks", "typo"}, "unknown check"},
		{[]string{"--checks", ""}, "check names must not be empty"},
		{[]string{"--concurrency", "0"}, "--concurrency must be positive"},
		{[]string{"--library", ""}, "library ID must not be empty"},
		{[]string{"--unknown"}, "flag provided but not defined"},
		{[]string{"extra"}, "Unexpected positional arguments"},
		{[]string{"--repair-timeout", "5s", "--include-seasons", "--include-episodes", "--verify-images", "--checks", "title", "--library", "movies", "--fail-on-issues"}, "requires terminal stdin and stdout"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			code, out, errout := invoke(t.Context(), "", append([]string{"tui"}, tc.args...)...)
			if code != 2 || !strings.Contains(errout, tc.message) || strings.Contains(out+errout, "cli-secret") {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, out, errout)
			}
		})
	}
}
