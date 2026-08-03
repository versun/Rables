// Package kv is a thin key/value store over the kv table, replacing the
// Rails.cache usages that must survive restarts (e.g. last comment fetch
// timestamps from ScheduledFetchSocialCommentsJob).
package kv

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"rables/internal/db/query"
)

// Store reads and writes kv rows.
type Store struct {
	q *query.Queries
}

// NewStore returns a Store backed by db.
func NewStore(db *sql.DB) *Store {
	return &Store{q: query.New(db)}
}

// Get returns the value for key; found is false when the key is absent.
func (s *Store) Get(ctx context.Context, key string) (value string, found bool, err error) {
	v, err := s.q.GetKVValue(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v.String, true, nil
}

// Set upserts key to value.
func (s *Store) Set(ctx context.Context, key, value string) error {
	now := time.Now().UTC().Unix()
	return s.q.SetKVValue(ctx, query.SetKVValueParams{
		Key:       key,
		Value:     sql.NullString{String: value, Valid: true},
		UpdatedAt: now,
	})
}
