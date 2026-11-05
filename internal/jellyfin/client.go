package jellyfin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	pageSize      = 200
	maxJSONBytes  = 32 << 20
	maxImageBytes = 4 << 20
)

type Client struct {
	base *url.URL
	key  string
	http *http.Client
}

// StatusError deliberately excludes response bodies, which can echo credentials.
type StatusError struct {
	StatusCode int
	Operation  string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: HTTP %d %s", e.Operation, e.StatusCode, http.StatusText(e.StatusCode))
}

func IsAuthError(err error) bool {
	e, ok := errors.AsType[*StatusError](err)
	return ok && (e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden)
}

func New(baseURL, apiKey string, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, errors.New("provide a valid http(s) Jellyfin server URL with --url or JELLYFIN_URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return nil, errors.New("the server URL must not contain credentials, query parameters, or a fragment")
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" || strings.ContainsAny(apiKey, "\"\\") || strings.ContainsFunc(apiKey, unicode.IsControl) {
		return nil, errors.New("provide a valid Jellyfin API key with --api-key or JELLYFIN_API_KEY")
	}
	if timeout <= 0 {
		return nil, errors.New("request timeout must be positive")
	}
	return &Client{base: u, key: apiKey, http: &http.Client{
		Timeout: timeout,
		// Do not forward the API key to redirect destinations, including subdomains.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) BaseURL() string { return strings.TrimRight(c.base.String(), "/") }

func (c *Client) ItemURL(itemID, serverID string) string {
	u := c.base.JoinPath("web", "index.html")
	u.Fragment = "/details?" + url.Values{"id": {itemID}, "serverId": {serverID}}.Encode()
	return u.String()
}

func (c *Client) Server(ctx context.Context) (ServerInfo, error) {
	var info ServerInfo
	if err := c.getJSON(ctx, "System/Info", nil, &info); err != nil {
		return info, err
	}
	if strings.TrimSpace(info.ID) == "" {
		return info, errors.New("System/Info: response is missing the server ID; check the server URL")
	}
	return info, nil
}

type queryResult[T any] struct {
	Items            *[]T `json:"Items"`
	TotalRecordCount *int `json:"TotalRecordCount"`
}

func (c *Client) Libraries(ctx context.Context) ([]Library, error) {
	var result queryResult[Library]
	if err := c.getJSON(ctx, "Library/MediaFolders", nil, &result); err != nil {
		return nil, err
	}
	if result.Items == nil {
		return nil, errors.New("Library/MediaFolders: response is missing Items")
	}
	seen := make(map[string]bool)
	libs := make([]Library, 0, len(*result.Items))
	for _, lib := range *result.Items {
		if !validID(lib.ID) {
			return nil, errors.New("Library/MediaFolders: invalid or missing library ID")
		}
		if seen[lib.ID] {
			continue
		}
		seen[lib.ID] = true
		if strings.TrimSpace(lib.Name) == "" {
			lib.Name = lib.ID
		}
		libs = append(libs, lib)
	}
	slices.SortFunc(libs, func(a, b Library) int {
		if n := strings.Compare(a.Name, b.Name); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	return libs, nil
}

// Items requests every field used by the audit, since absent optional Fields in
// Jellyfin responses otherwise look indistinguishable from missing metadata.
func (c *Client) Items(ctx context.Context, libraryID string, types []string, allImages bool) ([]Item, error) {
	if !validID(libraryID) {
		return nil, errors.New("invalid library ID")
	}
	if len(types) == 0 {
		return nil, errors.New("at least one item type is required")
	}
	q := url.Values{
		"ParentId": {libraryID}, "Recursive": {"true"},
		"IncludeItemTypes": {strings.Join(types, ",")},
		"Fields":           {"Overview,Genres,ProviderIds,Path,ParentId"},
		"EnableImages":     {"true"}, "EnableUserData": {"false"},
		"EnableTotalRecordCount": {"true"}, "CollapseBoxSetItems": {"false"},
		"IsMissing": {"false"}, "SortBy": {"SortName"}, "SortOrder": {"Ascending"},
		"Limit": {strconv.Itoa(pageSize)},
	}
	if !allImages {
		q.Set("ImageTypeLimit", "1")
	}
	return c.queryItems(ctx, q)
}

func (c *Client) queryItems(ctx context.Context, q url.Values) ([]Item, error) {
	var items []Item
	seen := make(map[string]bool)
	expectedTotal := -1
	for start := 0; ; {
		q.Set("StartIndex", strconv.Itoa(start))
		var page queryResult[Item]
		if err := c.getJSON(ctx, "Items", q, &page); err != nil {
			return nil, err
		}
		if page.Items == nil || page.TotalRecordCount == nil || *page.TotalRecordCount < 0 {
			return nil, errors.New("Items: response is missing Items or a valid TotalRecordCount")
		}
		if expectedTotal == -1 {
			expectedTotal = *page.TotalRecordCount
		}
		if *page.TotalRecordCount != expectedTotal {
			return nil, errors.New("Items: library count changed during pagination; retry after library updates finish")
		}
		batch := *page.Items
		if len(batch) == 0 {
			if start < *page.TotalRecordCount {
				return nil, errors.New("Items: empty page before the reported total; retry the scan")
			}
			break
		}
		if len(batch) > expectedTotal-start {
			return nil, errors.New("Items: page exceeds the reported total; refusing an incomplete scan")
		}
		for _, item := range batch {
			if !validID(item.ID) {
				return nil, errors.New("Items: invalid or missing item ID")
			}
			if seen[item.ID] {
				return nil, errors.New("Items: overlapping pages or repeated item IDs; retry after library updates finish")
			}
			seen[item.ID] = true
			items = append(items, item)
		}
		start += len(batch)
		if start >= *page.TotalRecordCount {
			break
		}
	}
	return items, nil
}

// VerifyImage returns a finding message for definite broken content, or an error
// for an operational failure that prevents deciding whether an image is usable.
func (c *Client) VerifyImage(ctx context.Context, ref ImageRef) (string, error) {
	if !validID(ref.ItemID) || (ref.Type != "Primary" && ref.Type != "Backdrop") || ref.Index < 0 {
		return "", errors.New("invalid image reference")
	}
	op := "Items/" + ref.ItemID + "/Images/" + ref.Type + "/" + strconv.Itoa(ref.Index)
	q := url.Values{"format": {"Png"}, "maxWidth": {"128"}, "maxHeight": {"128"}}
	if ref.Tag != "" {
		q.Set("tag", ref.Tag)
	}
	resp, err := c.get(ctx, op, q)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	label := ref.Type
	if ref.Type == "Backdrop" {
		label += " " + strconv.Itoa(ref.Index)
	}
	if resp.StatusCode == http.StatusNotFound {
		return label + " image reference returns HTTP 404", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", &StatusError{resp.StatusCode, op}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return "", fmt.Errorf("%s: read image: %s", op, c.redact(err.Error()))
	}
	if len(body) > maxImageBytes {
		return "", fmt.Errorf("%s: thumbnail exceeds the 4 MiB response limit", op)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		return label + " image response is not a decodable PNG, JPEG, or GIF", nil
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 512 || cfg.Height > 512 {
		return "", fmt.Errorf("%s: server did not return a bounded thumbnail", op)
	}
	if _, _, err := image.Decode(bytes.NewReader(body)); err != nil {
		return label + " image response contains invalid image data", nil
	}
	return "", nil
}

func (c *Client) getJSON(ctx context.Context, operation string, q url.Values, target any) error {
	resp, err := c.get(ctx, operation, q)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return &StatusError{resp.StatusCode, operation}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes+1))
	if err != nil {
		return fmt.Errorf("%s: read response: %s", operation, c.redact(err.Error()))
	}
	if len(body) > maxJSONBytes {
		return fmt.Errorf("%s: response exceeds the 32 MiB limit", operation)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("%s: invalid JSON response; check the Jellyfin server URL", operation)
	}
	return nil
}

func (c *Client) get(ctx context.Context, operation string, q url.Values) (*http.Response, error) {
	return c.request(ctx, http.MethodGet, operation, q)
}

func (c *Client) request(ctx context.Context, method, operation string, q url.Values) (*http.Response, error) {
	u := c.base.JoinPath(operation)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%s: create request: %s", operation, c.redact(err.Error()))
	}
	req.Header.Set("Authorization", "MediaBrowser Token=\""+c.key+"\"")
	req.Header.Set("Accept", "application/json, image/png")
	req.Header.Set("User-Agent", "jellyscout")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: request failed: %s", operation, c.redact(err.Error()))
	}
	return resp, nil
}

func (c *Client) redact(s string) string { return strings.ReplaceAll(s, c.key, "[REDACTED]") }

func validID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
