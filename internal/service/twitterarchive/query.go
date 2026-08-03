package twitterarchive

import (
	"context"

	"rables/internal/db/query"
)

// EntryTypes mirrors TwitterArchiveTweet::ENTRY_TYPES.
var EntryTypes = []string{EntryTypeTweet, EntryTypeReply, EntryTypeRetweetQuote}

// Tab is one public archive tab (TwitterArchivesController::TABS).
type Tab struct {
	Key   string
	Label string
}

// Tabs lists the public archive tabs in display order.
var Tabs = []Tab{
	{Key: EntryTypeTweet, Label: "Tweets"},
	{Key: EntryTypeReply, Label: "Replies"},
	{Key: EntryTypeRetweetQuote, Label: "Retweets / Quotes"},
	{Key: "like", Label: "Likes"},
}

// TabFor normalizes the ?tab= parameter like the Rails show action
// (TABS.key? check): unknown values fall back to "tweet".
func TabFor(value string) string {
	for _, tab := range Tabs {
		if tab.Key == value {
			return value
		}
	}
	return EntryTypeTweet
}

// IsTweetTab reports whether the tab lists archive tweets (vs likes).
func IsTweetTab(tab string) bool {
	for _, t := range EntryTypes {
		if t == tab {
			return true
		}
	}
	return false
}

// TweetURL mirrors TwitterArchiveTweet#tweet_url.
func TweetURL(tweet query.TwitterArchiveTweet) string {
	if tweet.ScreenName == "" || tweet.ScreenName == "archive" {
		return ""
	}
	return "https://twitter.com/" + tweet.ScreenName + "/status/" + tweet.TweetID
}

// Counts is the admin index summary (group-count queries of the Rails
// index action).
type Counts struct {
	Tweets       int64
	Replies      int64
	RetweetQuote int64
	Followers    int64
	Following    int64
	Likes        int64
	Total        int64
}

// LoadCounts mirrors the group(:entry_type)/group(:relationship_type) counts
// of Admin::TwitterArchivesController#index.
func LoadCounts(ctx context.Context, q *query.Queries) (Counts, error) {
	var c Counts
	tweetRows, err := q.CountTwitterArchiveTweetsByType(ctx)
	if err != nil {
		return c, err
	}
	for _, row := range tweetRows {
		switch row.EntryType {
		case EntryTypeTweet:
			c.Tweets = row.N
		case EntryTypeReply:
			c.Replies = row.N
		case EntryTypeRetweetQuote:
			c.RetweetQuote = row.N
		}
	}
	connRows, err := q.CountTwitterArchiveConnectionsByType(ctx)
	if err != nil {
		return c, err
	}
	for _, row := range connRows {
		switch row.RelationshipType {
		case "follower":
			c.Followers = row.N
		case "following":
			c.Following = row.N
		}
	}
	c.Likes, err = q.CountTwitterArchiveLikes(ctx)
	if err != nil {
		return c, err
	}
	c.Total = c.Tweets + c.Replies + c.RetweetQuote + c.Followers + c.Following + c.Likes
	return c, nil
}

// LastImportedAt mirrors TwitterArchiveImport.last_imported_at; ok is false
// when neither a completed import nor any archived tweet exists.
func LastImportedAt(ctx context.Context, q *query.Queries) (int64, bool) {
	v, err := q.TwitterArchiveLastImportedAt(ctx)
	if err != nil || v == nil {
		return 0, false
	}
	ts, ok := v.(int64)
	return ts, ok
}
