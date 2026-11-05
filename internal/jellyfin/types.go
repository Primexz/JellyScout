// Package jellyfin implements the Jellyfin API operations used by JellyScout.
package jellyfin

// Item contains only the Jellyfin fields needed by the audit.
type Item struct {
	ID                string            `json:"Id"`
	Name              string            `json:"Name"`
	Type              string            `json:"Type"`
	CollectionType    string            `json:"CollectionType"`
	Overview          string            `json:"Overview"`
	ProductionYear    *int              `json:"ProductionYear"`
	Genres            []string          `json:"Genres"`
	ProviderIDs       map[string]string `json:"ProviderIds"`
	ImageTags         map[string]string `json:"ImageTags"`
	BackdropImageTags []string          `json:"BackdropImageTags"`
	ParentID          string            `json:"ParentId"`
	Path              string            `json:"Path"`
	SeriesID          string            `json:"SeriesId"`
	SeriesName        string            `json:"SeriesName"`
	SeasonID          string            `json:"SeasonId"`
	SeasonName        string            `json:"SeasonName"`
	IndexNumber       *int              `json:"IndexNumber"`
	ParentIndexNumber *int              `json:"ParentIndexNumber"`
	LocationType      string            `json:"LocationType"`
	IsPlaceHolder     bool              `json:"IsPlaceHolder"`
	// Settings must be explicitly requested. Nil values represent unknown lock
	// settings, never an unlocked item; an explicit empty LockedFields is known.
	LockData     *bool    `json:"LockData"`
	LockedFields []string `json:"LockedFields"`
}

type ServerInfo struct {
	ID         string `json:"Id"`
	ServerName string `json:"ServerName"`
	Version    string `json:"Version"`
}

type Library struct {
	ID             string `json:"Id"`
	Name           string `json:"Name"`
	CollectionType string `json:"CollectionType"`
}

// ImageRef identifies an item's own image, including its original backdrop index.
type ImageRef struct {
	ItemID string
	Type   string
	Index  int
	Tag    string
}
