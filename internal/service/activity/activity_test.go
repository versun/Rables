package activity

import (
	"testing"

	"rables/internal/db"
	"rables/internal/db/query"
)

// TestLog verifies the stored row mirrors ActivityLog.log! normalization.
func TestLog(t *testing.T) {
	tests := []struct {
		name            string
		level           string
		action          string
		target          string
		description     string
		wantLevel       int64
		wantAction      string
		wantTarget      string
		wantDescription string
		wantDescValid   bool
	}{
		{name: "plain", level: "info", action: "created", target: "redirect", description: `regex="^/a$"`,
			wantLevel: LevelInfo, wantAction: "created", wantTarget: "redirect", wantDescription: `regex="^/a$"`, wantDescValid: true},
		{name: "warning alias", level: "warning", action: "synced", target: "twitter", description: "x",
			wantLevel: LevelWarn, wantAction: "synced", wantTarget: "twitter", wantDescription: "x", wantDescValid: true},
		{name: "error", level: "error", action: "failed", target: "static_file", description: "x",
			wantLevel: LevelError, wantAction: "failed", wantTarget: "static_file", wantDescription: "x", wantDescValid: true},
		{name: "unknown level falls back to warn", level: "notice", action: "x", target: "y", description: "x",
			wantLevel: LevelWarn, wantAction: "x", wantTarget: "y", wantDescription: "x", wantDescValid: true},
		{name: "blank level falls back to info", level: " ", action: "x", target: "y", description: "x",
			wantLevel: LevelInfo, wantAction: "x", wantTarget: "y", wantDescription: "x", wantDescValid: true},
		{name: "tokens normalized", level: "INFO", action: "Cross Post", target: "social-media", description: "",
			wantLevel: LevelInfo, wantAction: "cross_post", wantTarget: "social_media", wantDescription: "", wantDescValid: false},
		{name: "blank tokens become unknown", level: "info", action: "", target: " ", description: "x",
			wantLevel: LevelInfo, wantAction: "unknown", wantTarget: "unknown", wantDescription: "x", wantDescValid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, err := db.Open(t.TempDir())
			if err != nil {
				t.Fatalf("open db: %v", err)
			}
			t.Cleanup(func() { database.Close() })

			Log(t.Context(), database, tt.level, tt.action, tt.target, tt.description)

			rows, err := query.New(database).ListRecentActivityLogs(t.Context())
			if err != nil {
				t.Fatalf("list activity logs: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want 1", len(rows))
			}
			row := rows[0]
			if row.Level != tt.wantLevel {
				t.Errorf("level = %d, want %d", row.Level, tt.wantLevel)
			}
			if row.Action.String != tt.wantAction {
				t.Errorf("action = %q, want %q", row.Action.String, tt.wantAction)
			}
			if row.Target.String != tt.wantTarget {
				t.Errorf("target = %q, want %q", row.Target.String, tt.wantTarget)
			}
			if row.Description.Valid != tt.wantDescValid || row.Description.String != tt.wantDescription {
				t.Errorf("description = (%v, %q), want (%v, %q)",
					row.Description.Valid, row.Description.String, tt.wantDescValid, tt.wantDescription)
			}
		})
	}
}

// TestLogNeverFails: a broken database is reported internally, never to the
// caller (ActivityLog.log! rescues StandardError).
func TestLogNeverFails(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	database.Close()
	Log(t.Context(), database, "info", "created", "redirect", "x") // must not panic
}

// TestQuote mirrors ActivityLog.quote_string.
func TestQuote(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"plain", `"plain"`},
		{"  squish   me\t\n", `"squish me"`},
		{`say "hi"`, `"say \"hi\""`},
		{`back\slash`, `"back\\slash"`},
		{"", `""`},
	}
	for _, tt := range tests {
		if got := Quote(tt.in); got != tt.want {
			t.Errorf("Quote(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestLevelName covers the display mapping, including the unknown fallback.
func TestLevelName(t *testing.T) {
	for level, want := range map[int64]string{
		LevelInfo: "info", LevelWarn: "warn", LevelError: "error", 7: "unknown",
	} {
		if got := LevelName(level); got != want {
			t.Errorf("LevelName(%d) = %q, want %q", level, got, want)
		}
	}
}
