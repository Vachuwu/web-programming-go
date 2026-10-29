// Package handler translates HTTP requests into service calls.
package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"playlist/internal/domain"
	"playlist/internal/events"
	"playlist/internal/service"
)

type Handler struct {
	users     *service.UserService
	playlists *service.PlaylistService
	events    *events.Hub
}

func New(users *service.UserService, playlists ...*service.PlaylistService) *Handler {
	return NewWithEventHub(users, events.NewHub(), playlists...)
}

// NewWithEventHub allows the server and integration tests to own the EventHub
// lifecycle explicitly.
func NewWithEventHub(users *service.UserService, hub *events.Hub, playlists ...*service.PlaylistService) *Handler {
	handler := &Handler{users: users, events: hub}
	if len(playlists) > 0 {
		handler.playlists = playlists[0]
	}
	return handler
}

// EventHub returns the hub used by this HTTP handler.
func (h *Handler) EventHub() *events.Hub {
	return h.events
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /playlist", h.getPlaylist)
	mux.HandleFunc("GET /playlist/events", h.playlistEvents)
	mux.HandleFunc("POST /playlist/items", h.addPlaylistItem)
	mux.HandleFunc("POST /playlist/items/{id}/vote", h.vote)
	mux.HandleFunc("DELETE /playlist/items/{id}/vote", h.deleteVote)
	mux.HandleFunc("DELETE /playlist/items/{id}", h.removePlaylistItem)
	mux.HandleFunc("POST /playlist/play", h.play)
	mux.HandleFunc("POST /playlist/next", h.next)
	mux.HandleFunc("GET /playlist/history", h.getPlaylistHistory)
	mux.HandleFunc("POST /users", h.createUser)
	return mux
}

type createUserRequest struct {
	Name string `json:"name"`
}

type errorResponse struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (h *Handler) getPlaylist(w http.ResponseWriter, r *http.Request) {
	if h.playlists == nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Playlist service is unavailable")
		return
	}

	playlist, err := h.playlists.Playlist(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to read playlist")
		return
	}
	writeJSON(w, http.StatusOK, playlist)
}

type addPlaylistItemRequest struct {
	UserID int64   `json:"userId"`
	Title  string  `json:"title"`
	Artist string  `json:"artist"`
	URL    *string `json:"url"`
}

type voteRequest struct {
	UserID *int64 `json:"userId"`
	Value  *int16 `json:"value"`
}

type removePlaylistItemRequest struct {
	UserID *int64 `json:"userId"`
}

type playbackRequest struct {
	ExpectedVersion *int64 `json:"expectedVersion"`
}

func (h *Handler) addPlaylistItem(w http.ResponseWriter, r *http.Request) {
	if h.playlists == nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Playlist service is unavailable")
		return
	}

	var request addPlaylistItemRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Request body must contain valid JSON")
		return
	}

	item, err := h.playlists.AddItem(r.Context(), request.Title, request.Artist, request.URL, request.UserID)
	switch {
	case errors.Is(err, service.ErrTitleEmpty):
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Title must not be empty")
	case errors.Is(err, service.ErrArtistEmpty):
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Artist must not be empty")
	case errors.Is(err, service.ErrUserIDInvalid):
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "User ID must be positive")
	case errors.Is(err, service.ErrUserNotFound):
		writeError(w, http.StatusNotFound, "USER_NOT_FOUND", "User was not found")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to add playlist item")
	default:
		writeJSON(w, http.StatusCreated, item)
		h.publishItemEvent("item_added", item.ID)
	}
}

func (h *Handler) getPlaylistHistory(w http.ResponseWriter, r *http.Request) {
	if h.playlists == nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Playlist service is unavailable")
		return
	}

	history, err := h.playlists.History(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to read playlist history")
		return
	}
	writeJSON(w, http.StatusOK, history)
}

func (h *Handler) vote(w http.ResponseWriter, r *http.Request) {
	if h.playlists == nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Playlist service is unavailable")
		return
	}

	itemID, ok := parseItemID(w, r)
	if !ok {
		return
	}
	var request voteRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Request body must contain valid JSON")
		return
	}
	if request.UserID == nil || request.Value == nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "User ID and vote value are required")
		return
	}

	result, err := h.playlists.Vote(r.Context(), itemID, *request.UserID, *request.Value)
	if handleVoteResult(w, result, err) && result.Changed {
		h.publishItemEvent("vote_changed", itemID)
	}
}

func (h *Handler) deleteVote(w http.ResponseWriter, r *http.Request) {
	if h.playlists == nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Playlist service is unavailable")
		return
	}

	itemID, ok := parseItemID(w, r)
	if !ok {
		return
	}
	userID, err := strconv.ParseInt(r.URL.Query().Get("userId"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "User ID must be a positive number")
		return
	}

	result, err := h.playlists.DeleteVote(r.Context(), itemID, userID)
	if handleVoteResult(w, result, err) && result.Changed {
		h.publishItemEvent("vote_changed", itemID)
	}
}

func (h *Handler) removePlaylistItem(w http.ResponseWriter, r *http.Request) {
	if h.playlists == nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Playlist service is unavailable")
		return
	}

	itemID, ok := parseItemID(w, r)
	if !ok {
		return
	}
	var request removePlaylistItemRequest
	if err := decodeJSON(r, &request); err != nil || request.UserID == nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "User ID is required")
		return
	}

	item, err := h.playlists.Remove(r.Context(), itemID, *request.UserID)
	switch {
	case errors.Is(err, service.ErrItemIDInvalid), errors.Is(err, service.ErrUserIDInvalid):
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Item and user IDs must be positive")
	case errors.Is(err, service.ErrUserNotFound):
		writeError(w, http.StatusNotFound, "USER_NOT_FOUND", "User was not found")
	case errors.Is(err, service.ErrItemNotFound):
		writeError(w, http.StatusNotFound, "ITEM_NOT_FOUND", "Playlist item was not found")
	case errors.Is(err, service.ErrItemNotQueued):
		writeError(w, http.StatusConflict, "ITEM_NOT_QUEUED", "Only queued items can be removed")
	case errors.Is(err, service.ErrItemAccessDenied):
		writeError(w, http.StatusForbidden, "ITEM_ACCESS_DENIED", "Only the item author can remove it")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to remove playlist item")
	default:
		writeJSON(w, http.StatusOK, item)
		h.publishItemEvent("item_removed", item.ID)
	}
}

func (h *Handler) play(w http.ResponseWriter, r *http.Request) {
	h.changePlayback(w, r, false)
}

func (h *Handler) next(w http.ResponseWriter, r *http.Request) {
	h.changePlayback(w, r, true)
}

func (h *Handler) changePlayback(w http.ResponseWriter, r *http.Request, next bool) {
	if h.playlists == nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Playlist service is unavailable")
		return
	}

	var request playbackRequest
	if err := decodeJSON(r, &request); err != nil || request.ExpectedVersion == nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Expected playback version is required")
		return
	}

	var (
		snapshot any
		err      error
	)
	if next {
		snapshot, err = h.playlists.Next(r.Context(), *request.ExpectedVersion)
	} else {
		snapshot, err = h.playlists.Play(r.Context(), *request.ExpectedVersion)
	}

	switch {
	case errors.Is(err, service.ErrVersionInvalid):
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Expected playback version must be positive")
	case errors.Is(err, service.ErrVersionConflict):
		writeError(w, http.StatusConflict, "VERSION_CONFLICT", "Playback state has changed")
	case errors.Is(err, service.ErrAlreadyPlaying):
		writeError(w, http.StatusConflict, "ALREADY_PLAYING", "A playlist item is already playing")
	case errors.Is(err, service.ErrPlaylistEmpty):
		writeError(w, http.StatusConflict, "PLAYLIST_EMPTY", "Playlist queue is empty")
	case errors.Is(err, service.ErrNothingPlaying):
		writeError(w, http.StatusConflict, "NOTHING_PLAYING", "No playlist item is playing")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to change playback")
	default:
		writeJSON(w, http.StatusOK, snapshot)
		if playlist, ok := snapshot.(domain.PlaylistSnapshot); ok {
			h.publishPlaybackEvent(playlist.PlaybackVersion)
		}
	}
}

func parseItemID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	itemID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || itemID <= 0 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Item ID must be a positive number")
		return 0, false
	}
	return itemID, true
}

func handleVoteResult(w http.ResponseWriter, result domain.VoteResult, err error) bool {
	switch {
	case errors.Is(err, service.ErrItemIDInvalid), errors.Is(err, service.ErrUserIDInvalid):
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Item and user IDs must be positive")
	case errors.Is(err, service.ErrVoteInvalid):
		writeError(w, http.StatusBadRequest, "INVALID_VOTE", "Vote must be -1 or 1")
	case errors.Is(err, service.ErrUserNotFound):
		writeError(w, http.StatusNotFound, "USER_NOT_FOUND", "User was not found")
	case errors.Is(err, service.ErrItemNotFound):
		writeError(w, http.StatusNotFound, "ITEM_NOT_FOUND", "Playlist item was not found")
	case errors.Is(err, service.ErrItemNotQueued):
		writeError(w, http.StatusConflict, "ITEM_NOT_QUEUED", "Votes are allowed only for queued items")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to change vote")
	default:
		writeJSON(w, http.StatusOK, result)
		return true
	}
	return false
}

type itemEventPayload struct {
	ItemID int64 `json:"itemId"`
}

type playbackEventPayload struct {
	PlaybackVersion int64 `json:"playbackVersion"`
}

func (h *Handler) publishItemEvent(name string, itemID int64) {
	if h.events != nil {
		h.events.Publish(events.Event{Name: name, Data: itemEventPayload{ItemID: itemID}})
	}
}

func (h *Handler) publishPlaybackEvent(playbackVersion int64) {
	if h.events != nil {
		h.events.Publish(events.Event{Name: "playback_changed", Data: playbackEventPayload{PlaybackVersion: playbackVersion}})
	}
}

func (h *Handler) playlistEvents(w http.ResponseWriter, r *http.Request) {
	if h.events == nil {
		writeError(w, http.StatusServiceUnavailable, "INTERNAL_ERROR", "Event service is unavailable")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Streaming is unsupported")
		return
	}
	subscription, unsubscribe := h.events.Subscribe()
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-subscription:
			if !ok {
				return
			}
			if err := writeSSEEvent(w, event); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeSSEEvent(w io.Writer, event events.Event) error {
	data, err := json.Marshal(event.Data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Name, data)
	return err
}

func (h *Handler) createUser(w http.ResponseWriter, r *http.Request) {
	var request createUserRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Request body must contain valid JSON")
		return
	}

	user, err := h.users.Create(r.Context(), request.Name)
	if errors.Is(err, service.ErrUserNameEmpty) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Name must not be empty")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to create user")
		return
	}
	writeJSON(w, http.StatusCreated, user)
}

func decodeJSON(r *http.Request, destination any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain a single JSON value")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: apiError{Code: code, Message: message}})
}
