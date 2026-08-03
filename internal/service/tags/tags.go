// Package tags holds the shared tag domain logic ported from
// app/models/tag.rb: case-insensitive find-or-create by names, slug
// generation, and the rename/destroy side effects.
package tags

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"rables/internal/db/query"
	"rables/internal/domain"
)

// Validation failures mirroring Tag's model validations; handlers map these
// to a 422 form re-render.
var (
	ErrNameBlank = errors.New("name can't be blank")
	ErrNameTaken = errors.New("name has already been taken")
)

// FindOrCreateByNames ports Tag.find_or_create_by_names: names are trimmed,
// blanks dropped, and duplicates collapsed (case-insensitively, keeping
// first-occurrence order), then each name maps to exactly one tag — looked up
// case-insensitively or created. A creator losing the UNIQUE(name) race
// re-reads the winner's row, like the RecordNotUnique rescue in the source.
// It returns the tag ids in the order the names first appeared.
func FindOrCreateByNames(ctx context.Context, q *query.Queries, names []string) ([]int64, error) {
	var ids []int64
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		tag, err := findOrCreate(ctx, q, name)
		if err != nil {
			return nil, err
		}
		ids = append(ids, tag.ID)
	}
	return ids, nil
}

// Create inserts one tag after the model validations: blank or
// case-insensitively duplicate names are rejected; the slug is generated like
// Tag#generate_slug. Concurrent creators get ErrNameTaken.
func Create(ctx context.Context, q *query.Queries, name string) (query.Tag, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return query.Tag{}, ErrNameBlank
	}
	if _, err := q.GetTagByLowerName(ctx, name); err == nil {
		return query.Tag{}, ErrNameTaken
	} else if !errors.Is(err, sql.ErrNoRows) {
		return query.Tag{}, err
	}
	now := time.Now().Unix()
	for {
		slug, err := uniqueSlug(ctx, q, name, 0)
		if err != nil {
			return query.Tag{}, err
		}
		tag, err := q.CreateTag(ctx, query.CreateTagParams{Name: name, Slug: slug, CreatedAt: now, UpdatedAt: now})
		if err == nil {
			return tag, nil
		}
		if !isUniqueViolation(err) {
			return query.Tag{}, err
		}
		// Lost a race: the conflicting row is visible now, so the next loop
		// either reports the taken name or generates the next slug suffix.
		if _, lookErr := q.GetTagByLowerName(ctx, name); lookErr == nil {
			return query.Tag{}, ErrNameTaken
		}
	}
}

// Rename ports Admin::TagsController#update plus Tag#touch_articles: the
// rename and the bump of every tagged article's updated_at (the render-cache
// key, spec §4.4) commit in one transaction. The slug is kept, because
// Tag#generate_slug only fills blank slugs. Touching happens only when the
// name actually changed (saved_change_to_name?).
func Rename(ctx context.Context, db *sql.DB, id int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return ErrNameBlank
	}
	q := query.New(db)
	tag, err := q.GetTagByID(ctx, id)
	if err != nil {
		return err
	}
	if clash, err := q.GetTagByLowerName(ctx, name); err == nil && clash.ID != id {
		return ErrNameTaken
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	qtx := q.WithTx(tx)
	now := time.Now().Unix()
	if err := qtx.UpdateTagName(ctx, query.UpdateTagNameParams{Name: name, UpdatedAt: now, ID: id}); err != nil {
		if isUniqueViolation(err) {
			return ErrNameTaken
		}
		return err
	}
	if tag.Name != name {
		if err := qtx.TouchArticlesByTagID(ctx, query.TouchArticlesByTagIDParams{UpdatedAt: now, TagID: id}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Destroy ports tag.destroy with dependent: :destroy on the join tables:
// article_tags and subscriber_tags rows go first, then the tag, all in one
// transaction. A missing tag reports sql.ErrNoRows, like Tag.find.
func Destroy(ctx context.Context, db *sql.DB, id int64) error {
	q := query.New(db)
	if _, err := q.GetTagByID(ctx, id); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	qtx := q.WithTx(tx)
	if err := qtx.DeleteArticleTagsByTagID(ctx, id); err != nil {
		return err
	}
	if err := qtx.DeleteSubscriberTagsByTagID(ctx, id); err != nil {
		return err
	}
	if err := qtx.DeleteTag(ctx, id); err != nil {
		return err
	}
	return tx.Commit()
}

// findOrCreate is the race-safe single-name version behind
// FindOrCreateByNames.
func findOrCreate(ctx context.Context, q *query.Queries, name string) (query.Tag, error) {
	if tag, err := q.GetTagByLowerName(ctx, name); err == nil {
		return tag, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return query.Tag{}, err
	}
	now := time.Now().Unix()
	for {
		slug, err := uniqueSlug(ctx, q, name, 0)
		if err != nil {
			return query.Tag{}, err
		}
		err = q.InsertTagIfAbsent(ctx, query.InsertTagIfAbsentParams{Name: name, Slug: slug, CreatedAt: now, UpdatedAt: now})
		if err != nil {
			// Only the slug can still conflict (the name is absorbed by
			// ON CONFLICT); loop to generate the next suffix.
			if isUniqueViolation(err) {
				continue
			}
			return query.Tag{}, err
		}
		// Whether this call inserted the row or a concurrent creator won the
		// name race, the row is now committed and readable.
		return q.GetTagByLowerName(ctx, name)
	}
}

// uniqueSlug ports Tag#generate_slug: the squished name itself is the slug
// (the source deliberately keeps non-ASCII scripts, e.g. Chinese, instead of
// parameterizing), and "-1", "-2", ... suffixes are appended while the
// candidate is taken by another tag.
func uniqueSlug(ctx context.Context, q *query.Queries, name string, excludeID int64) (string, error) {
	base := domain.Squish(name)
	candidate := base
	for counter := 1; ; counter++ {
		taken, err := q.CountTagsBySlug(ctx, query.CountTagsBySlugParams{Slug: candidate, ID: excludeID})
		if err != nil {
			return "", err
		}
		if taken == 0 {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s-%d", base, counter)
	}
}

// isUniqueViolation reports a SQLite UNIQUE constraint failure.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
