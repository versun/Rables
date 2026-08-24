// Package activity mirrors the Rails ActivityLog model: a fire-and-forget
// audit trail written to the activity_logs table. Logging must never break
// the main flow, so Log swallows and reports its own errors.
package activity

import (
	"context"
	"database/sql"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"rables/internal/db/query"
)

// Level values stored in activity_logs.level (Rails enum :level).
const (
	LevelInfo  int64 = 0
	LevelWarn  int64 = 1
	LevelError int64 = 2
)

// levelNames maps the normalized level token to its stored value.
var levelNames = map[string]int64{"info": LevelInfo, "warn": LevelWarn, "error": LevelError}

// levelAliases mirrors ActivityLog::LEVEL_ALIASES.
var levelAliases = map[string]string{"warning": "warn"}

// jobRunIDKey carries the running job_runs id from the worker to handlers, so
// the rows Log writes during a job run link back to it (see WithJobRunID).
type jobRunIDKey struct{}

// WithJobRunID returns a ctx that marks Log rows written under it as belonging
// to job run id. The jobs worker wraps the handler ctx with it.
func WithJobRunID(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, jobRunIDKey{}, id)
}

// jobRunID reports the job run id carried by ctx, if any.
func jobRunID(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(jobRunIDKey{}).(int64)
	return id, ok
}

// Log records one activity row, mirroring ActivityLog.log!: action/target are
// normalized tokens, level accepts the Rails names plus aliases, and an empty
// description is stored as NULL. When the ctx carries a job run id (the worker
// sets it via WithJobRunID), the row links back to that run. It never returns
// an error.
func Log(ctx context.Context, db *sql.DB, level, action, target, description string) {
	now := time.Now().Unix()
	var runID sql.NullInt64
	if id, ok := jobRunID(ctx); ok {
		runID = sql.NullInt64{Int64: id, Valid: true}
	}
	err := query.New(db).CreateActivityLog(ctx, query.CreateActivityLogParams{
		Level:       NormalizeLevel(level),
		Action:      sql.NullString{String: NormalizeToken(action), Valid: true},
		Target:      sql.NullString{String: NormalizeToken(target), Valid: true},
		Description: sql.NullString{String: description, Valid: description != ""},
		JobRunID:    runID,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err != nil {
		slog.Default().Warn("activity.Log failed", "error", err)
	}
}

// NormalizeToken mirrors ActivityLog.normalize_token: trimmed, lowercased,
// dashes/whitespace collapsed to underscores, blank becomes "unknown".
func NormalizeToken(value string) string {
	token := strings.TrimSpace(value)
	if token == "" {
		return "unknown"
	}
	return strings.ToLower(normalizeSepRe.ReplaceAllString(token, "_"))
}

// normalizeSepRe matches the runs of dashes/whitespace normalize_token folds.
var normalizeSepRe = regexp.MustCompile(`[-\s]+`)

// NormalizeLevel mirrors ActivityLog.normalize_level: aliases resolve, a blank
// token falls back to info, and unknown names fall back to warn.
func NormalizeLevel(value string) int64 {
	normalized := NormalizeToken(value)
	if alias, ok := levelAliases[normalized]; ok {
		normalized = alias
	}
	if level, ok := levelNames[normalized]; ok {
		return level
	}
	if normalized == "unknown" {
		return LevelInfo
	}
	slog.Default().Warn("activity: unknown level, defaulting to warn", "level", value)
	return LevelWarn
}

// LevelName maps a stored level value back to its token for display, like the
// Rails enum reader; unknown values render as "unknown".
func LevelName(level int64) string {
	switch level {
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	}
	return "unknown"
}

// Quote mirrors ActivityLog.quote_string: the text is squished, backslashes
// and double quotes escaped, and the result wrapped in double quotes.
func Quote(text string) string {
	s := strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
