package articles

import (
	"reflect"
	"testing"
)

func TestParseStatus(t *testing.T) {
	tests := []struct {
		in   string
		want int
		ok   bool
	}{
		{"draft", 0, true},
		{"publish", 1, true},
		{"schedule", 2, true},
		{"trash", 3, true},
		{"shared", 4, true},
		{"", 0, false},
		{"bogus", 0, false},
	}
	for _, tt := range tests {
		got, ok := ParseStatus(tt.in)
		if ok != tt.ok || int(got) != tt.want {
			t.Errorf("ParseStatus(%q) = %d, %v; want %d, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestParseScheduledPlatforms(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"json array", `["twitter","mastodon"]`, []string{"mastodon", "twitter"}},
		{"empty json", `[]`, nil},
		{"unknown dropped", `["mastodon","myspace"]`, []string{"mastodon"}},
		{"duplicates collapse", `["bluesky","bluesky"]`, []string{"bluesky"}},
		{"comma fallback", "twitter, bluesky", []string{"twitter", "bluesky"}},
		{"garbage", "not-json", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseScheduledPlatforms(tt.raw); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseScheduledPlatforms(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestSplitTagList(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"  ", nil},
		{"go", []string{"go"}},
		{"go, web ,x", []string{"go", " web ", "x"}}, // trimming happens downstream
	}
	for _, tt := range tests {
		if got := SplitTagList(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("SplitTagList(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestSelectedPlatforms(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]bool
		want []string
	}{
		{"nil map", nil, []string{}},
		{"order normalized", map[string]bool{"bluesky": true, "mastodon": true}, []string{"mastodon", "bluesky"}},
		{"false dropped", map[string]bool{"mastodon": true, "twitter": false}, []string{"mastodon"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SelectedPlatforms(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SelectedPlatforms(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
