// Package service contains the application's business rules.
package service

import (
	"context"
	"errors"
	"strings"

	"playlist/internal/domain"
	"playlist/internal/repository"
)

var ErrUserNameEmpty = errors.New("user name must not be empty")

type UserService struct {
	users *repository.UserRepository
}

func NewUserService(users *repository.UserRepository) *UserService {
	return &UserService{users: users}
}

func (s *UserService) Create(ctx context.Context, name string) (domain.User, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return domain.User{}, ErrUserNameEmpty
	}
	return s.users.Create(ctx, name)
}
