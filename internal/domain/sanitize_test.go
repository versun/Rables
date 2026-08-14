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
			name:        "iframe src must be absolute http(s)",
			in:          `<iframe src="javascript:alert(1)"></iframe><iframe src="//evil.com/embed"></iframe><iframe src="/admin/posts"></iframe><iframe src="https://v.com/e"></iframe>`,
			contains:    []string{`src="https://v.com/e"`},
			notContains: []string{"javascript", "evil.com", "/admin/posts"},
		},
		{
			// url.Parse accepts these as scheme http(s) with an empty host,
			// but browsers resolve them against the embedding origin.
			name:        "iframe src with scheme but no host stripped",
			in:          `<iframe src="https:/admin"></iframe><iframe src="https:admin"></iframe>`,
			notContains: []string{"src=", "admin"},
		},
		{
			name:        "video/audio/source src must be http(s) or /files/ root-relative",
			in:          `<video controls src="ftp://v.com/a.mp4"></video><video src="/files/a.mp4" controls></video><video src="/local/a.mp4"></video><audio src="/files/a.mp3"></audio><source src="https://v.com/a.mp4">`,
			contains:    []string{"controls", `src="/files/a.mp4"`, `src="/files/a.mp3"`, `src="https://v.com/a.mp4"`},
			notContains: []string{"ftp://", "/local/a.mp4"},
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

func TestSanitizeHTMLActionTextAttachments(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		contains    []string
		notContains []string
	}{
		{
			name:     "image attachment becomes img with caption alt and dimensions",
			in:       `<p>a</p><action-text-attachment url="/files/abc123" content-type="image/png" filename="pic.png" caption="My pic" width="800" height="600"></action-text-attachment><p>b</p>`,
			contains: []string{`<p>a</p>`, `<p>b</p>`, `<img`, `src="/files/abc123"`, `alt="My pic"`, `width="800"`, `height="600"`},
		},
		{
			name:     "image without caption falls back to filename alt",
			in:       `<action-text-attachment url="/files/abc123" content-type="image/jpeg" filename="photo.jpg"></action-text-attachment>`,
			contains: []string{`<img`, `src="/files/abc123"`, `alt="photo.jpg"`},
		},
		{
			name:     "non-image attachment becomes download link",
			in:       `<action-text-attachment url="/files/def456" content-type="application/pdf" filename="doc.pdf"></action-text-attachment>`,
			contains: []string{`<a href="/files/def456">doc.pdf</a>`},
		},
		{
			name:        "svg attachment is a link, never an inline img",
			in:          `<action-text-attachment url="/files/ghi789" content-type="image/svg+xml" filename="logo.svg"></action-text-attachment>`,
			contains:    []string{`<a href="/files/ghi789">logo.svg</a>`},
			notContains: []string{"<img"},
		},
		{
			name:        "attachment without url is dropped",
			in:          `<p>a</p><action-text-attachment content-type="image/png" filename="x.png"></action-text-attachment><p>b</p>`,
			contains:    []string{`<p>a</p>`, `<p>b</p>`},
			notContains: []string{"action-text-attachment", "<img"},
		},
		{
			name:        "no attachments passes through unchanged",
			in:          `<p>plain</p><img src="/files/x.png" alt="x">`,
			contains:    []string{`<p>plain</p>`, `src="/files/x.png"`},
			notContains: []string{"action-text-attachment"},
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
			if strings.Contains(got, "action-text-attachment") {
				t.Errorf("SanitizeHTML(%q) = %q, want no action-text-attachment left", tt.in, got)
			}
			for _, unwanted := range tt.notContains {
				if strings.Contains(got, unwanted) {
					t.Errorf("SanitizeHTML(%q) = %q, want it not to contain %q", tt.in, got, unwanted)
				}
			}
		})
	}
}

func TestDeepNesting(t *testing.T) {
	// x/net/html refuses an open-element stack over 512 nodes; the helpers
	// must take their parse-error fallback instead of panicking.
	deep := strings.Repeat("<div>", 600)
	if got := SanitizeHTML(deep); !strings.Contains(got, "<div>") {
		t.Errorf("SanitizeHTML(600 nested divs) = %q, want sanitized content preserved", got)
	}
	if got := AddLazyLoading(deep); got != deep {
		t.Errorf("AddLazyLoading(600 nested divs) = %q, want input unchanged", got)
	}
}

func TestDeepNestingFallback(t *testing.T) {
	// 513 nested divs exceed x/net/html's 512-node open-element stack, so
	// ParseFragment fails and restrictMediaSrc takes its tokenizer fallback.
	// The fallback applies the same allowedMediaSrc rule as the DOM pass:
	// the same-origin iframe src is stripped while the /files/ video src is
	// kept, and the surrounding content passes through.
	raw := strings.Repeat("<div>", 513) +
		`<iframe src="/admin"></iframe><video src="/files/a.mp4" controls></video>`
	got := SanitizeHTML(raw)
	if strings.Contains(got, "/admin") {
		t.Errorf("SanitizeHTML(deep nesting + media) = %q, want %q stripped", got, "/admin")
	}
	for _, want := range []string{"<div>", "<iframe></iframe>", `src="/files/a.mp4"`, "controls"} {
		if !strings.Contains(got, want) {
			t.Errorf("SanitizeHTML(deep nesting + media) = %q, want it to contain %q", got, want)
		}
	}
}

func TestOversizedInputSkipsDOMPasses(t *testing.T) {
	// Over maxHTMLParseBytes the ParseFragment-based passes are skipped so no
	// DOM is built: bluemonday still sanitizes (script stripped) and lazy
	// loading does not run, but restrictMediaSrc takes its tokenizer
	// fallback — the same allowedMediaSrc rule applies, so the same-origin
	// iframe src is stripped while the /files/ video src is kept.
	big := strings.Repeat("<p>x</p>", maxHTMLParseBytes/len("<p>x</p>")+1)

	raw := `<iframe src="/admin"></iframe><video src="/files/a.mp4" controls></video><script>alert(1)</script>` + big
	got := SanitizeHTML(raw)
	if strings.Contains(got, "script") {
		t.Errorf("SanitizeHTML(oversized) kept script markup: %.100q", got)
	}
	if strings.Contains(got, "/admin") {
		t.Errorf("SanitizeHTML(oversized) kept the same-origin iframe src: %.100q", got)
	}
	for _, want := range []string{"<iframe></iframe>", `src="/files/a.mp4"`} {
		if !strings.Contains(got, want) {
			t.Errorf("SanitizeHTML(oversized) = %.100q, want it to contain %q", got, want)
		}
	}

	rawImg := `<img src="a.png">` + big
	if got := AddLazyLoading(rawImg); got != rawImg {
		t.Errorf("AddLazyLoading(oversized) should return the input unchanged")
	}
}
