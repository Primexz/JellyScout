package jellyfin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c, err := New(server.URL+"/jellyfin", "test-api-secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestItemsPaginationAndFields(t *testing.T) {
	var starts []string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/jellyfin/Items" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != `MediaBrowser Token="test-api-secret"` {
			t.Errorf("authorization = %q", got)
		}
		q := r.URL.Query()
		for k, v := range map[string]string{
			"ParentId": "lib", "Recursive": "true", "IncludeItemTypes": "Movie,Series",
			"Fields": "Overview,Genres,ProviderIds,Path,ParentId", "EnableImages": "true",
			"ImageTypeLimit": "1", "CollapseBoxSetItems": "false", "IsMissing": "false",
			"EnableUserData": "false", "EnableTotalRecordCount": "true", "Limit": "200",
		} {
			if q.Get(k) != v {
				t.Errorf("%s = %q; want %q", k, q.Get(k), v)
			}
		}
		starts = append(starts, q.Get("StartIndex"))
		var ids []string
		switch q.Get("StartIndex") {
		case "0":
			ids = []string{"a", "b"}
		case "2":
			ids = []string{"c", "d"}
		case "4":
			ids = []string{"e"}
		default:
			t.Errorf("unexpected start: %s", q.Get("StartIndex"))
		}
		items := make([]Item, 0, len(ids))
		for _, id := range ids {
			items = append(items, Item{ID: id, Type: "Movie"})
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"Items": items, "TotalRecordCount": 5}); err != nil {
			t.Errorf("encode items response: %v", err)
		}
	})
	items, err := c.Items(t.Context(), "lib", []string{"Movie", "Series"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 5 || !slices.Equal(starts, []string{"0", "2", "4"}) {
		t.Fatalf("items=%d starts=%v", len(items), starts)
	}
}

func TestItemsPaginationGuards(t *testing.T) {
	for _, tt := range []struct {
		name          string
		first, second string
		count         int
		wantError     bool
	}{
		{"overlapping pages", `{"Items":[{"Id":"a"},{"Id":"b"}],"TotalRecordCount":4}`, `{"Items":[{"Id":"b"},{"Id":"c"}],"TotalRecordCount":4}`, 0, true},
		{"changing total", `{"Items":[{"Id":"a"}],"TotalRecordCount":2}`, `{"Items":[{"Id":"b"}],"TotalRecordCount":3}`, 0, true},
		{"count too small", `{"Items":[{"Id":"a"},{"Id":"b"}],"TotalRecordCount":1}`, "", 0, true},
		{"repeated page", `{"Items":[{"Id":"a"}],"TotalRecordCount":2}`, `{"Items":[{"Id":"a"}],"TotalRecordCount":2}`, 0, true},
		{"premature empty", `{"Items":[{"Id":"a"}],"TotalRecordCount":2}`, `{"Items":[],"TotalRecordCount":2}`, 0, true},
		{"empty library", `{"Items":[],"TotalRecordCount":0}`, "", 0, false},
		{"missing items", `{"TotalRecordCount":0}`, "", 0, true},
		{"missing total", `{"Items":[]}`, "", 0, true},
		{"invalid id", `{"Items":[{"Id":"../secret"}],"TotalRecordCount":1}`, "", 0, true},
		{"invalid json", `<html>login</html>`, "", 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Has("ImageTypeLimit") {
					t.Error("deep scan must retrieve all backdrops")
				}
				if r.URL.Query().Get("StartIndex") == "0" {
					if _, err := fmt.Fprint(w, tt.first); err != nil {
						t.Errorf("write first page response: %v", err)
					}
				} else {
					if _, err := fmt.Fprint(w, tt.second); err != nil {
						t.Errorf("write second page response: %v", err)
					}
				}
			})
			got, err := c.Items(t.Context(), "lib", []string{"Movie"}, true)
			if (err != nil) != tt.wantError {
				t.Fatalf("error=%v wantError=%v", err, tt.wantError)
			}
			if err == nil && len(got) != tt.count {
				t.Fatalf("got %d items; want %d", len(got), tt.count)
			}
		})
	}
}

func TestConnectionValidation(t *testing.T) {
	for _, tt := range []struct {
		url, key string
		timeout  time.Duration
	}{
		{"", "secret", time.Second}, {"ftp://localhost", "secret", time.Second},
		{"https://user:pass@localhost", "secret", time.Second},
		{"https://localhost?api_key=secret", "secret", time.Second},
		{"https://localhost#fragment", "secret", time.Second},
		{"https://localhost", "", time.Second}, {"https://localhost", "secret\r\nInjected: yes", time.Second},
		{"https://localhost", `bad"key`, time.Second}, {"https://localhost", "secret", 0},
	} {
		if _, err := New(tt.url, tt.key, tt.timeout); err == nil {
			t.Errorf("accepted invalid connection: %q", tt.url)
		}
	}
}

func TestLibrariesAndServerInfo(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jellyfin/System/Info":
			if _, err := fmt.Fprint(w, `{"Id":"server","ServerName":"Home","Version":"10.11"}`); err != nil {
				t.Errorf("write server info response: %v", err)
			}
		case "/jellyfin/Library/MediaFolders":
			if _, err := fmt.Fprint(w, `{"Items":[{"Id":"tv","Name":"TV","CollectionType":"tvshows"},{"Id":"movies","Name":"Movies"},{"Id":"tv","Name":"Duplicate"}]}`); err != nil {
				t.Errorf("write libraries response: %v", err)
			}
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
	})
	info, err := c.Server(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "server" || info.ServerName != "Home" {
		t.Fatalf("info=%+v", info)
	}
	libs, err := c.Libraries(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(libs) != 2 || libs[0].ID != "movies" || libs[1].ID != "tv" {
		t.Fatalf("libraries=%+v", libs)
	}
	u, err := url.Parse(c.ItemURL("movie-id", "server"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/jellyfin/web/index.html" || !strings.HasPrefix(u.Fragment, "/details?") {
		t.Fatalf("item URL=%s", u)
	}
	q, err := url.ParseQuery(strings.TrimPrefix(u.Fragment, "/details?"))
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("id") != "movie-id" || q.Get("serverId") != "server" || strings.Contains(u.String(), "test-api-secret") {
		t.Fatalf("item URL=%s", u)
	}
}

func TestAuthFailuresNeverIncludeResponseBody(t *testing.T) {
	for _, status := range []int{401, 403, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				if _, err := fmt.Fprint(w, "test-api-secret is rejected"); err != nil {
					t.Errorf("write error response: %v", err)
				}
			})
			_, err := c.Server(t.Context())
			if err == nil || strings.Contains(err.Error(), "test-api-secret") {
				t.Fatalf("unsafe error: %v", err)
			}
			if got := IsAuthError(fmt.Errorf("wrapped: %w", err)); got != (status == 401 || status == 403) {
				t.Fatalf("auth error=%v", got)
			}
		})
	}
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestVerifyImages(t *testing.T) {
	valid := pngBytes(t)
	for _, tt := range []struct {
		name            string
		status          int
		body            []byte
		broken, opError bool
	}{
		{"valid", 200, valid, false, false},
		{"404", 404, nil, true, false},
		{"html", 200, []byte("<html>oops</html>"), true, false},
		{"truncated", 200, valid[:len(valid)-10], true, false},
		{"server failure", 500, []byte("test-api-secret"), false, true},
		{"rate limited", 429, nil, false, true},
		{"auth", 401, nil, false, true},
		{"oversized", 200, bytes.Repeat([]byte("x"), maxImageBytes+1), false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/jellyfin/Items/movie/Images/Backdrop/2" {
					t.Errorf("path=%s", r.URL.Path)
				}
				q := r.URL.Query()
				if q.Get("format") != "Png" || q.Get("maxWidth") != "128" || q.Get("tag") != "image-tag" {
					t.Errorf("query=%v", q)
				}
				w.WriteHeader(tt.status)
				if _, err := w.Write(tt.body); err != nil {
					t.Errorf("write image response: %v", err)
				}
			})
			problem, err := c.VerifyImage(t.Context(), ImageRef{ItemID: "movie", Type: "Backdrop", Index: 2, Tag: "image-tag"})
			if (err != nil) != tt.opError || (problem != "") != tt.broken {
				t.Fatalf("problem=%q err=%v", problem, err)
			}
			if err != nil && strings.Contains(err.Error(), "test-api-secret") {
				t.Fatalf("unsafe error=%v", err)
			}
		})
	}
}

func TestTimeoutCancellationAndRedirects(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		c.http.Timeout = 20 * time.Millisecond
		if _, err := c.Server(t.Context()); err == nil {
			t.Fatal("expected timeout")
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		var hits atomic.Int32
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := c.Server(ctx); err == nil || hits.Load() != 0 {
			t.Fatalf("err=%v hits=%d", err, hits.Load())
		}
	})
	t.Run("redirect credentials", func(t *testing.T) {
		var hits atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
		defer target.Close()
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) })
		_, err := c.Server(t.Context())
		if err == nil || hits.Load() != 0 {
			t.Fatalf("err=%v redirected hits=%d", err, hits.Load())
		}
	})
}
