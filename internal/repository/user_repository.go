// Package repository contains SQL access to persistent data.
package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"playlist/internal/domain"
)

type UserRepository struct {
	pool *pgxpool.Pool
}

func NewUserRepository(pool *pgxpool.Pool) *UserRepository {
	return &UserRepository{pool: pool}
}

func (r *UserRepository) Create(ctx context.Context, name string) (domain.User, error) {
	const query = `
		INSERT INTO users (name)
		VALUES ($1)
		RETURNING id, name, created_at`

	var user domain.User
	if err := r.pool.QueryRow(ctx, query, name).Scan(&user.ID, &user.Name, &user.CreatedAt); err != nil {
		return domain.User{}, fmt.Errorf("create user: %w", err)
	}
	return user, nil
}
