package templates

import (
	"strconv"
	"strings"
	"testing"
)

func TestFormatTime(t *testing.T) {
	const layout = "2006-01-02 15:04:05"
	tests := []struct {
		name string
		unix int64
		tz   string
		want string
	}{
		{"epoch in UTC", 0, "UTC", "1970-01-01 00:00:00"},
		{"new york is UTC-5 (EST)", 1700000000, "America/New_York", "2023-11-14 17:13:20"},
		{"shanghai is UTC+8 and next day", 1700000000, "Asia/Shanghai", "2023-11-15 06:13:20"},
		{"new york DST is UTC-4 (EDT)", 1689000000, "America/New_York", "2023-07-10 10:40:00"},
		{"invalid zone falls back to UTC", 1700000000, "Not/AZone", "2023-11-14 22:13:20"},
		{"empty zone falls back to UTC", 1700000000, "", "2023-11-14 22:13:20"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatTime(tt.unix, tt.tz, layout); got != tt.want {
				t.Errorf("FormatTime(%d, %q) = %q, want %q", tt.unix, tt.tz, got, tt.want)
			}
		})
	}
}

func TestFormatTimeLayoutPassthrough(t *testing.T) {
	if got := FormatTime(1700000000, "UTC", "2006-01-02"); got != "2023-11-14" {
		t.Errorf("date-only layout = %q, want 2023-11-14", got)
	}
}

func windowString(items []PageItem) string {
	parts := make([]string, len(items))
	for i, it := range items {
		if it.Gap {
			parts[i] = "…"
		} else {
			parts[i] = strconv.Itoa(it.Page)
		}
	}
	return strings.Join(parts, " ")
}

func TestPaginationWindow(t *testing.T) {
	tests := []struct {
		name    string
		current int
		total   int
		want    string
	}{
		{"no pages", 1, 0, ""},
		{"single page", 1, 1, "1"},
		{"few pages all visible", 3, 5, "1 2 3 4 5"},
		{"first page of many", 1, 20, "1 2 3 4 5 6 7 8 9 … 19 20"},
		{"left run touches middle", 7, 20, "1 2 3 4 5 6 7 8 9 10 11 … 19 20"},
		{"left gap boundary", 8, 20, "1 2 3 4 5 6 7 8 9 10 11 12 … 19 20"},
		{"left gap opens", 9, 20, "1 2 … 5 6 7 8 9 10 11 12 13 … 19 20"},
		{"gaps on both sides", 10, 20, "1 2 … 6 7 8 9 10 11 12 13 14 … 19 20"},
		{"right gap closes", 13, 20, "1 2 … 9 10 11 12 13 14 15 16 17 18 19 20"},
		{"right run touches middle", 14, 20, "1 2 … 10 11 12 13 14 15 16 17 18 19 20"},
		{"last page of many", 20, 20, "1 2 … 12 13 14 15 16 17 18 19 20"},
		{"current clamped above total", 99, 3, "1 2 3"},
		{"current clamped below one", 0, 3, "1 2 3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := windowString(PaginationWindow(tt.current, tt.total)); got != tt.want {
				t.Errorf("PaginationWindow(%d, %d) = %q, want %q", tt.current, tt.total, got, tt.want)
			}
		})
	}
}

func TestFlashHTML(t *testing.T) {
	tests := []struct {
		name  string
		flash Flash
		want  []string
		omit  []string
	}{
		{"notice only", Flash{Notice: "saved"},
			[]string{`<div class="flash flash-notice">saved</div>`}, []string{"flash-alert"}},
		{"alert only", Flash{Alert: "boom"},
			[]string{`<div class="flash flash-alert">boom</div>`}, []string{"flash-notice"}},
		{"both", Flash{Notice: "n", Alert: "a"},
			[]string{"flash-notice", "flash-alert"}, nil},
		{"empty renders nothing", Flash{},
			nil, []string{"flash"}},
		{"message is escaped", Flash{Notice: "<b>x</b>"},
			[]string{"&lt;b&gt;x&lt;/b&gt;"}, []string{"<b>x</b>"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(FlashHTML(tt.flash))
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("FlashHTML(%+v) missing %q, got %q", tt.flash, want, got)
				}
			}
			for _, omit := range tt.omit {
				if strings.Contains(got, omit) {
					t.Errorf("FlashHTML(%+v) should not contain %q, got %q", tt.flash, omit, got)
				}
			}
		})
	}
}
