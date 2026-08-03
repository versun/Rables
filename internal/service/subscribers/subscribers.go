// Package subscribers holds the subscriber domain logic ported from
// app/models/subscriber.rb (plan section 4.6): email validation, token
// generation, state predicates, tag replacement, and the newsletter
// recipient selection.
package subscribers

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"regexp"
	"time"

	"rables/internal/db/query"
)

// emailRE mirrors URI::MailTo::EMAIL_REGEXP (anchored, RE2-compatible).
var emailRE = regexp.MustCompile(`^[a-zA-Z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`)

// ValidEmail reports whether email passes the Subscriber format validation.
func ValidEmail(email string) bool { return emailRE.MatchString(email) }

// NewToken mirrors SecureRandom.urlsafe_base64(32): 32 random bytes encoded
// base64url without padding (43 chars).
func NewToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Confirmed mirrors Subscriber#confirmed?.
func Confirmed(sub query.Subscriber) bool { return sub.ConfirmedAt.Valid }

// Active mirrors Subscriber#active?: confirmed and not unsubscribed.
func Active(sub query.Subscriber) bool { return Confirmed(sub) && !sub.UnsubscribedAt.Valid }

// Create inserts a subscriber, filling blank tokens like
// Subscriber#generate_tokens (pre-set tokens, e.g. from imports, are kept).
func Create(ctx context.Context, q *query.Queries, email, confirmationToken, unsubscribeToken string) (query.Subscriber, error) {
	var err error
	if confirmationToken == "" {
		if confirmationToken, err = NewToken(); err != nil {
			return query.Subscriber{}, err
		}
	}
	if unsubscribeToken == "" {
		if unsubscribeToken, err = NewToken(); err != nil {
			return query.Subscriber{}, err
		}
	}
	now := time.Now().UTC().Unix()
	return q.CreateSubscriber(ctx, query.CreateSubscriberParams{
		Email:             email,
		ConfirmationToken: sql.NullString{String: confirmationToken, Valid: true},
		UnsubscribeToken:  sql.NullString{String: unsubscribeToken, Valid: true},
		CreatedAt:         now,
		UpdatedAt:         now,
	})
}

// ReplaceTags mirrors subscriber.tags = tags: the join rows are rebuilt to
// exactly the given tag ids (an empty list unsubscribes from everything
// specific, i.e. "all content").
func ReplaceTags(ctx context.Context, q *query.Queries, subscriberID int64, tagIDs []int64) error {
	if err := q.DeleteSubscriberTagsBySubscriberID(ctx, subscriberID); err != nil {
		return err
	}
	now := time.Now().UTC().Unix()
	for _, tagID := range tagIDs {
		if err := q.AddSubscriberTag(ctx, query.AddSubscriberTagParams{
			SubscriberID: subscriberID,
			TagID:        tagID,
			CreatedAt:    now,
			UpdatedAt:    now,
		}); err != nil {
			return err
		}
	}
	return nil
}

// Destroy mirrors subscriber.destroy with dependent: :destroy on
// subscriber_tags, committed in one transaction.
func Destroy(ctx context.Context, db *sql.DB, id int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	qtx := query.New(db).WithTx(tx)
	if err := qtx.DeleteSubscriberTagsBySubscriberID(ctx, id); err != nil {
		return err
	}
	if err := qtx.DeleteSubscriber(ctx, id); err != nil {
		return err
	}
	return tx.Commit()
}

// Recipient is one subscriber as seen by newsletter sending.
type Recipient struct {
	ID     int64
	Email  string
	Active bool    // Subscriber#active?
	TagIDs []int64 // empty = subscribed to all content
}

// FilterRelevant mirrors the recipient selection of
// NativeNewsletterSenderJob#perform: only active subscribers; a subscriber
// with no tags receives every article, otherwise at least one article tag
// must intersect the subscriber's tags.
func FilterRelevant(subs []Recipient, articleTagIDs []int64) []Recipient {
	var out []Recipient
	for _, sub := range subs {
		if !sub.Active {
			continue
		}
		if len(sub.TagIDs) == 0 {
			out = append(out, sub)
			continue
		}
		for _, tagID := range sub.TagIDs {
			if containsID(articleTagIDs, tagID) {
				out = append(out, sub)
				break
			}
		}
	}
	return out
}

func containsID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
