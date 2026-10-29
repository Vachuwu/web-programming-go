package domain

import "time"

type ItemStatus string

const (
	StatusQueued  ItemStatus = "queued"
	StatusPlaying ItemStatus = "playing"
	StatusPlayed  ItemStatus = "played"
	StatusRemoved ItemStatus = "removed"
)

type PlaylistItem struct {
	ID        int64      `json:"id"`
	Title     string     `json:"title"`
	Artist    string     `json:"artist"`
	URL       *string    `json:"url,omitempty"`
	Status    ItemStatus `json:"status"`
	Score     int64      `json:"score"`
	CreatedAt time.Time  `json:"createdAt"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	PlayedAt  *time.Time `json:"playedAt,omitempty"`
	AddedBy   int64      `json:"-"`
}

type PlaylistSnapshot struct {
	Playing         *PlaylistItem  `json:"playing"`
	Queue           []PlaylistItem `json:"queue"`
	PlaybackVersion int64          `json:"playbackVersion"`
}

// VoteResult is the current aggregate state returned after a vote operation.
// Value is omitted when a vote was deleted.
type VoteResult struct {
	ItemID int64  `json:"itemId"`
	UserID int64  `json:"userId"`
	Value  *int16 `json:"value,omitempty"`
	Score  int64  `json:"score"`
	Changed bool  `json:"-"`
}
