package service

import (
	"context"
	"errors"
	"strings"

	"playlist/internal/domain"
	"playlist/internal/repository"
)

var (
	ErrTitleEmpty      = errors.New("track title must not be empty")
	ErrArtistEmpty     = errors.New("track artist must not be empty")
	ErrUserIDInvalid   = errors.New("user ID must be positive")
	ErrItemIDInvalid   = errors.New("playlist item ID must be positive")
	ErrVoteInvalid     = errors.New("vote must be -1 or 1")
	ErrUserNotFound    = errors.New("user not found")
	ErrItemNotFound    = errors.New("playlist item not found")
	ErrItemNotQueued   = errors.New("playlist item is not queued")
	ErrItemAccessDenied = errors.New("playlist item access denied")
	ErrVersionInvalid  = errors.New("expected playback version must be positive")
	ErrVersionConflict = errors.New("playback version conflict")
	ErrAlreadyPlaying  = errors.New("playlist already has a playing item")
	ErrPlaylistEmpty   = errors.New("playlist queue is empty")
	ErrNothingPlaying  = errors.New("playlist has no playing item")
)

type PlaylistService struct {
	playlist *repository.PlaylistRepository
}

func NewPlaylistService(playlist *repository.PlaylistRepository) *PlaylistService {
	return &PlaylistService{playlist: playlist}
}

func (s *PlaylistService) AddItem(ctx context.Context, title, artist string, url *string, userID int64) (domain.PlaylistItem, error) {
	title = strings.TrimSpace(title)
	artist = strings.TrimSpace(artist)
	if title == "" {
		return domain.PlaylistItem{}, ErrTitleEmpty
	}
	if artist == "" {
		return domain.PlaylistItem{}, ErrArtistEmpty
	}
	if userID <= 0 {
		return domain.PlaylistItem{}, ErrUserIDInvalid
	}

	var item domain.PlaylistItem
	err := s.playlist.WithPlaylistLock(ctx, func(tx *repository.PlaylistTransaction) error {
		var addErr error
		item, addErr = tx.Add(ctx, title, artist, url, userID)
		return addErr
	})
	if errors.Is(err, repository.ErrUserNotFound) {
		return domain.PlaylistItem{}, ErrUserNotFound
	}
	return item, err
}

func (s *PlaylistService) Playlist(ctx context.Context) (domain.PlaylistSnapshot, error) {
	return s.playlist.Playlist(ctx)
}

func (s *PlaylistService) History(ctx context.Context) ([]domain.PlaylistItem, error) {
	return s.playlist.History(ctx)
}

func (s *PlaylistService) Play(ctx context.Context, expectedVersion int64) (domain.PlaylistSnapshot, error) {
	return s.changePlayback(ctx, expectedVersion, false)
}

func (s *PlaylistService) Next(ctx context.Context, expectedVersion int64) (domain.PlaylistSnapshot, error) {
	return s.changePlayback(ctx, expectedVersion, true)
}

func (s *PlaylistService) changePlayback(ctx context.Context, expectedVersion int64, next bool) (domain.PlaylistSnapshot, error) {
	if expectedVersion <= 0 {
		return domain.PlaylistSnapshot{}, ErrVersionInvalid
	}

	var snapshot domain.PlaylistSnapshot
	err := s.playlist.WithPlaylistLock(ctx, func(tx *repository.PlaylistTransaction) error {
		var changeErr error
		if next {
			snapshot, changeErr = tx.Next(ctx, expectedVersion)
		} else {
			snapshot, changeErr = tx.Play(ctx, expectedVersion)
		}
		return changeErr
	})
	switch {
	case errors.Is(err, repository.ErrVersionConflict):
		return domain.PlaylistSnapshot{}, ErrVersionConflict
	case errors.Is(err, repository.ErrAlreadyPlaying):
		return domain.PlaylistSnapshot{}, ErrAlreadyPlaying
	case errors.Is(err, repository.ErrPlaylistEmpty):
		return domain.PlaylistSnapshot{}, ErrPlaylistEmpty
	case errors.Is(err, repository.ErrNothingPlaying):
		return domain.PlaylistSnapshot{}, ErrNothingPlaying
	default:
		return snapshot, err
	}
}

func (s *PlaylistService) Vote(ctx context.Context, itemID, userID int64, value int16) (domain.VoteResult, error) {
	if err := validateVoteInput(itemID, userID); err != nil {
		return domain.VoteResult{}, err
	}
	if value != -1 && value != 1 {
		return domain.VoteResult{}, ErrVoteInvalid
	}
	return s.vote(ctx, itemID, userID, value, false)
}

func (s *PlaylistService) DeleteVote(ctx context.Context, itemID, userID int64) (domain.VoteResult, error) {
	if err := validateVoteInput(itemID, userID); err != nil {
		return domain.VoteResult{}, err
	}
	return s.vote(ctx, itemID, userID, 0, true)
}

func (s *PlaylistService) Remove(ctx context.Context, itemID, userID int64) (domain.PlaylistItem, error) {
	if err := validateVoteInput(itemID, userID); err != nil {
		return domain.PlaylistItem{}, err
	}

	var item domain.PlaylistItem
	err := s.playlist.WithPlaylistLock(ctx, func(tx *repository.PlaylistTransaction) error {
		var removeErr error
		item, removeErr = tx.Remove(ctx, itemID, userID)
		return removeErr
	})
	switch {
	case errors.Is(err, repository.ErrUserNotFound):
		return domain.PlaylistItem{}, ErrUserNotFound
	case errors.Is(err, repository.ErrItemNotFound):
		return domain.PlaylistItem{}, ErrItemNotFound
	case errors.Is(err, repository.ErrItemNotQueued):
		return domain.PlaylistItem{}, ErrItemNotQueued
	case errors.Is(err, repository.ErrItemAccessDenied):
		return domain.PlaylistItem{}, ErrItemAccessDenied
	default:
		return item, err
	}
}

func (s *PlaylistService) vote(ctx context.Context, itemID, userID int64, value int16, deleteVote bool) (domain.VoteResult, error) {
	var (
		result domain.VoteResult
		err    error
	)
	err = s.playlist.WithPlaylistLock(ctx, func(tx *repository.PlaylistTransaction) error {
		if deleteVote {
			result, err = tx.DeleteVote(ctx, itemID, userID)
			return err
		}
		result, err = tx.Vote(ctx, itemID, userID, value)
		return err
	})
	switch {
	case errors.Is(err, repository.ErrUserNotFound):
		return domain.VoteResult{}, ErrUserNotFound
	case errors.Is(err, repository.ErrItemNotFound):
		return domain.VoteResult{}, ErrItemNotFound
	case errors.Is(err, repository.ErrItemNotQueued):
		return domain.VoteResult{}, ErrItemNotQueued
	default:
		return result, err
	}
}

func validateVoteInput(itemID, userID int64) error {
	if itemID <= 0 {
		return ErrItemIDInvalid
	}
	if userID <= 0 {
		return ErrUserIDInvalid
	}
	return nil
}
