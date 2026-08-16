package contentmigrate

import (
	"strings"
	"testing"

	"golang.org/x/net/html"

	"rables/internal/domain"
)

// render runs the standard write path every markdown article goes through.
func render(t *testing.T, md string) string {
	t.Helper()
	return domain.SanitizeHTML(domain.RenderMarkdown(md))
}

func convert(t *testing.T, body string) string {
	t.Helper()
	md, err := Convert(body)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	return md
}

func TestConvertPlainHTML(t *testing.T) {
	md := convert(t, `<div class="trix-content"><p>Hello <strong>bold</strong> world</p><ul><li>one</li><li>two</li></ul></div>`)
	for _, want := range []string{"Hello **bold** world", "- one", "- two"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "trix-content") {
		t.Errorf("wrapper divs should be unwrapped:\n%s", md)
	}
	out := render(t, md)
	if !strings.Contains(out, "<strong>bold</strong>") {
		t.Errorf("rendered output lost bold: %q", out)
	}
}

// A closing ** preceded by fullwidth punctuation and followed by CJK text is
// not right-flanking per CommonMark, so the span must stay raw HTML.
func TestConvertCJKFlanking(t *testing.T) {
	md := convert(t, `<p>入门方法：<strong>为您使用的工具做出贡献：</strong>想想您日常使用的工具。</p>`)
	if strings.Contains(md, "**为您使用的工具做出贡献：**想") {
		t.Fatalf("raw ** delimiters would render literally:\n%s", md)
	}
	out := render(t, md)
	if !strings.Contains(out, "<strong>为您使用的工具做出贡献：</strong>想想") {
		t.Errorf("rendered output lost the bold span: %q", out)
	}
	if strings.Contains(out, "**") {
		t.Errorf("rendered output shows literal asterisks: %q", out)
	}
}

// Adjacent bold+italic must not fuse into one emphasis run.
func TestConvertAdjacentBoldItalic(t *testing.T) {
	md := convert(t, `<div><strong>环境：</strong><em>macOS 15.7.3</em></div>`)
	out := render(t, md)
	if !strings.Contains(out, "<strong>环境：</strong><em>macOS 15.7.3</em>") {
		t.Errorf("adjacent bold/italic fused or mangled: %q\nmd:\n%s", out, md)
	}
}

func TestConvertImageWithSizeStaysRaw(t *testing.T) {
	md := convert(t, `<p><img src="/files/abc123" width="768" height="340"/></p>`)
	if !strings.Contains(md, `<img src="/files/abc123" width="768" height="340"/>`) {
		t.Errorf("sized image should stay raw HTML:\n%s", md)
	}
	out := render(t, md)
	if !strings.Contains(out, `width="768"`) {
		t.Errorf("rendered output lost the size attrs: %q", out)
	}
}

func TestConvertPlainImageBecomesMarkdown(t *testing.T) {
	md := convert(t, `<p><img src="/files/abc123" alt="pic"/></p>`)
	if !strings.Contains(md, `![pic](/files/abc123)`) {
		t.Errorf("plain image should become markdown:\n%s", md)
	}
}

func TestConvertVideoStaysRaw(t *testing.T) {
	md := convert(t, `<p>看看这个</p><video src="/files/v123" controls></video>`)
	out := render(t, md)
	if !strings.Contains(out, `<video src="/files/v123" controls=""></video>`) {
		t.Errorf("video did not survive: %q\nmd:\n%s", out, md)
	}
}

// An img inside pre renders in browsers; the pre must stay raw HTML so the
// image is not lost.
func TestConvertPreWithImageStaysRaw(t *testing.T) {
	md := convert(t, `<pre>还内置了 10 个模版<br/><img src="/files/o7i45" width="986" height="433"/></pre>`)
	out := render(t, md)
	if !strings.Contains(out, `<img src="/files/o7i45" width="986" height="433"/>`) {
		t.Errorf("img inside pre lost: %q\nmd:\n%s", out, md)
	}
}

// Blank lines inside a raw pre are escaped as &#10; so the markdown HTML
// block does not end early. The rendered output must keep the exact newline
// run: the entity decodes to a newline, nothing more.
func TestConvertRawPreBlankLines(t *testing.T) {
	md := convert(t, `<pre><em>line1</em>

line3</pre>`)
	if strings.Contains(md, "\n\nline3") {
		t.Errorf("blank line inside raw pre would end the HTML block:\n%s", md)
	}
	out := render(t, md)
	if !strings.Contains(out, "line1") || !strings.Contains(out, "line3") {
		t.Errorf("pre content lost: %q\nmd:\n%s", out, md)
	}
	if !strings.Contains(out, "<em>line1</em>") {
		t.Errorf("inline style inside pre lost: %q", out)
	}
	if got := newlinesBetween(t, out, "line1", "line3"); got != 2 {
		t.Errorf("rendered newlines between line1 and line3 = %d, want 2 (out %q)", got, out)
	}
}

// The blank-line escaping must also reach text nested inside an element
// subtree of a raw pre (html.Render serializes the whole subtree).
func TestConvertRawPreNestedBlankLines(t *testing.T) {
	md := convert(t, `<pre><em>line1

line3</em><img src="/files/x1" width="10" height="20"/></pre>`)
	if strings.Contains(md, "\n\nline3") {
		t.Errorf("nested blank line inside raw pre would end the HTML block:\n%s", md)
	}
	out := render(t, md)
	if !strings.Contains(out, "line3") || !strings.Contains(out, `<img src="/files/x1" width="10" height="20"/>`) {
		t.Errorf("nested pre content lost: %q\nmd:\n%s", out, md)
	}
	if got := newlinesBetween(t, out, "line1", "line3"); got != 2 {
		t.Errorf("rendered newlines between line1 and line3 = %d, want 2 (out %q)", got, out)
	}
}

// newlinesBetween unescapes out and counts the newline run between the two
// markers (the &#10; escaping must render back to exactly the source run).
func newlinesBetween(t *testing.T, out, from, to string) int {
	t.Helper()
	plain := html.UnescapeString(out)
	i := strings.Index(plain, from)
	j := strings.Index(plain, to)
	if i < 0 || j < 0 || j < i {
		t.Fatalf("markers %q/%q not found in order in %q", from, to, plain)
	}
	return strings.Count(plain[i+len(from):j], "\n")
}

// Underline and insert have no Markdown syntax; they must stay raw inline
// HTML instead of being silently unwrapped.
func TestConvertUnderlineAndInsertStayRaw(t *testing.T) {
	md := convert(t, `<p>a <u>under</u> and <ins>new</ins> b</p>`)
	out := render(t, md)
	if !strings.Contains(out, "<u>under</u>") {
		t.Errorf("underline unwrapped: %q\nmd:\n%s", out, md)
	}
	if !strings.Contains(out, "<ins>new</ins>") {
		t.Errorf("insert unwrapped: %q\nmd:\n%s", out, md)
	}
}

// Semantic inline tags the sanitizer allows must stay raw: sub/sup (H₂O is
// not H2O) and ruby annotation (rt is not body text).
func TestConvertSemanticInlineStaysRaw(t *testing.T) {
	md := convert(t, `<p>H<sub>2</sub>O and x<sup>2</sup> <ruby>汉<rt>han</rt></ruby> <abbr title="HyperText">HTML</abbr> <small>fine</small> <time datetime="2026-01-01">Jan</time></p>`)
	out := render(t, md)
	// datetime is not in the sanitizer attribute whitelist (mirrors Rails), so
	// only the tag itself survives — like on every write path.
	for _, want := range []string{"<sub>2</sub>", "<sup>2</sup>", "<ruby>汉<rt>han</rt></ruby>", `<abbr title="HyperText">HTML</abbr>`, "<small>fine</small>", "<time>Jan</time>"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output lost %q: %q\nmd:\n%s", want, out, md)
		}
	}
}

// Spans/divs with style or class keep raw HTML (styling the sanitizer
// allows); plain ones unwrap. The retired editor's trix-content wrapper
// unwraps regardless of its class.
func TestConvertStyledSpanAndDiv(t *testing.T) {
	md := convert(t, `<p>a <span style="color:red">red</span> b</p><div class="note">noted</div><div class="trix-content"><p>wrapped</p></div>`)
	out := render(t, md)
	if !strings.Contains(out, `<span style="color:red">red</span>`) {
		t.Errorf("styled span lost its style: %q\nmd:\n%s", out, md)
	}
	if !strings.Contains(out, `<div class="note">`) {
		t.Errorf("classed div unwrapped: %q\nmd:\n%s", out, md)
	}
	if strings.Contains(out, "trix-content") {
		t.Errorf("editor wrapper should be unwrapped: %q\nmd:\n%s", out, md)
	}
}

// Definition lists have no Markdown syntax; keep them raw.
func TestConvertDefinitionListStaysRaw(t *testing.T) {
	md := convert(t, `<dl><dt>term</dt><dd>def</dd></dl>`)
	out := render(t, md)
	if !strings.Contains(out, "<dl>") || !strings.Contains(out, "<dt>term</dt>") || !strings.Contains(out, "<dd>def</dd>") {
		t.Errorf("definition list flattened: %q\nmd:\n%s", out, md)
	}
}

// A body that converts to nothing yields "", so callers store NULL instead
// of a lone newline.
func TestConvertBlankInput(t *testing.T) {
	for _, in := range []string{"", "  ", "<p></p>"} {
		if md := convert(t, in); md != "" {
			t.Errorf("Convert(%q) = %q, want empty", in, md)
		}
	}
}

// Attachments pointing at pre-migration Rails URLs are dead (404); the
// element renders nothing today, so it is dropped.
func TestConvertDeadAttachmentDropped(t *testing.T) {
	md := convert(t, `<p>before</p><action-text-attachment url="/rails/active_storage/blobs/redirect/eyJ--x" content-type="image/png" filename="a.png"></action-text-attachment><p>after</p>`)
	if strings.Contains(md, "action-text-attachment") || strings.Contains(md, "active_storage") {
		t.Errorf("dead attachment should be dropped:\n%s", md)
	}
	out := render(t, md)
	if !strings.Contains(out, "before") || !strings.Contains(out, "after") {
		t.Errorf("surrounding content lost: %q", out)
	}
}

func TestConvertTableBecomesGFM(t *testing.T) {
	md := convert(t, `<table><thead><tr><th>a</th><th>b</th></tr></thead><tbody><tr><td>1</td><td>2</td></tr></tbody></table>`)
	if !strings.Contains(md, "| a | b |") {
		t.Errorf("table should become GFM syntax:\n%s", md)
	}
	out := render(t, md)
	if !strings.Contains(out, "<table>") || !strings.Contains(out, "<td>2</td>") {
		t.Errorf("GFM table did not render back: %q", out)
	}
}

func TestConvertEmptyHeadingStaysRaw(t *testing.T) {
	md := convert(t, `<p>a</p><h3></h3><p>b</p>`)
	if !strings.Contains(md, "<h3>") {
		t.Errorf("empty heading should stay raw HTML:\n%s", md)
	}
}

func TestConvertCodeBlock(t *testing.T) {
	md := convert(t, `<pre><code class="language-bash">brew install ollama
ollama serve &amp;</code></pre>`)
	if !strings.Contains(md, "```") || !strings.Contains(md, "ollama serve &") {
		t.Errorf("code block mangled:\n%s", md)
	}
	out := render(t, md)
	if !strings.Contains(out, "ollama serve &amp;") {
		t.Errorf("code block content lost: %q", out)
	}
}
