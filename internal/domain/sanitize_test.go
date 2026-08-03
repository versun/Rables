package domain

import (
	"strings"
	"testing"
)

func TestSanitizeHTML(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		contains    []string
		notContains []string
	}{
		{
			name:        "blank input",
			in:          "  ",
			notContains: []string{"<"},
		},
		{
			name:        "script stripped with content",
			in:          `<p>hi</p><script>alert(1)</script>`,
			contains:    []string{"<p>hi</p>"},
			notContains: []string{"script", "alert"},
		},
		{
			name:     "allowed tags and attributes kept",
			in:       `<h1 class="t" style="color:red">T</h1><a href="https://x.com" target="_blank" rel="n">l</a><table><tr><td colspan="2">c</td></tr></table><iframe src="https://v.com/e" allowfullscreen></iframe>`,
			contains: []string{`<h1`, `class="t"`, `style="color:red"`, `href="https://x.com"`, `target="_blank"`, `rel="n"`, `colspan="2"`, `<table>`, `src="https://v.com/e"`, `allowfullscreen`},
		},
		{
			name:        "event handler attribute stripped",
			in:          `<p onclick="evil()">hi</p>`,
			contains:    []string{"<p>hi</p>"},
			notContains: []string{"onclick"},
		},
		{
			name:        "javascript href dropped",
			in:          `<a href="javascript:alert(1)">x</a>`,
			notContains: []string{"javascript", "href"},
		},
		{
			name:     "mailto and relative links kept",
			in:       `<a href="mailto:x@y.com">m</a><a href="/about">r</a>`,
			contains: []string{`href="mailto:x@y.com"`, `href="/about"`},
		},
		{
			name:     "relative img src kept",
			in:       `<img src="/files/abc.png" alt="a">`,
			contains: []string{`src="/files/abc.png"`, `alt="a"`},
		},
		{
			name:        "iframe src must be http(s)",
			in:          `<iframe src="javascript:alert(1)"></iframe><iframe src="/local/embed"></iframe>`,
			notContains: []string{"javascript", "src"},
		},
		{
			name:        "video/audio/source src must be http(s)",
			in:          `<video controls src="ftp://v.com/a.mp4"></video><audio src="/local/a.mp3"></audio><source src="https://v.com/a.mp4">`,
			contains:    []string{"controls", `src="https://v.com/a.mp4"`},
			notContains: []string{"ftp://", "/local/a.mp3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeHTML(tt.in)
			for _, want := range tt.contains {
				if !strings.Contains(got, want) {
					t.Errorf("SanitizeHTML(%q) = %q, want it to contain %q", tt.in, got, want)
				}
			}
			for _, unwanted := range tt.notContains {
				if strings.Contains(got, unwanted) {
					t.Errorf("SanitizeHTML(%q) = %q, want it not to contain %q", tt.in, got, unwanted)
				}
			}
		})
	}
}

func TestSanitizeHTMLEmpty(t *testing.T) {
	if got := SanitizeHTML(""); got != "" {
		t.Errorf("SanitizeHTML(\"\") = %q, want \"\"", got)
	}
}

func TestAddLazyLoading(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		contains    []string
		notContains []string
	}{
		{
			name:     "img without loading gets lazy",
			in:       `<p>x</p><img src="a.png">`,
			contains: []string{`<img src="a.png" loading="lazy"/>`},
		},
		{
			name:        "existing loading kept",
			in:          `<img src="a.png" loading="eager">`,
			contains:    []string{`loading="eager"`},
			notContains: []string{"lazy"},
		},
		{
			name:     "no img unchanged",
			in:       `<p>no img</p>`,
			contains: []string{"<p>no img</p>"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AddLazyLoading(tt.in)
			for _, want := range tt.contains {
				if !strings.Contains(got, want) {
					t.Errorf("AddLazyLoading(%q) = %q, want it to contain %q", tt.in, got, want)
				}
			}
			for _, unwanted := range tt.notContains {
				if strings.Contains(got, unwanted) {
					t.Errorf("AddLazyLoading(%q) = %q, want it not to contain %q", tt.in, got, unwanted)
				}
			}
		})
	}

	if got := AddLazyLoading(""); got != "" {
		t.Errorf("AddLazyLoading(\"\") = %q, want \"\"", got)
	}
}
