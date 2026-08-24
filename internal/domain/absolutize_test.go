package domain

import (
	"strings"
	"testing"
)

func TestAbsolutizeURLs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		base string
		want string // empty means the input passes through unchanged
	}{
		{
			name: "root-relative img src and file href absolutized",
			in:   `<p><img src="/files/a.png" alt="a"></p><a href="/files/doc.pdf">d</a>`,
			base: "https://blog.example.com",
			want: `<p><img src="https://blog.example.com/files/a.png" alt="a"/></p><a href="https://blog.example.com/files/doc.pdf">d</a>`,
		},
		{
			name: "base trailing slash chomped",
			in:   `<img src="/files/a.png">`,
			base: "https://blog.example.com/",
			want: `<img src="https://blog.example.com/files/a.png"/>`,
		},
		{
			name: "absolute and protocol-relative URLs untouched",
			in:   `<img src="https://cdn.example.com/a.png"><img src="//cdn.example.com/b.png"><a href="https://x.com">x</a>`,
			base: "https://blog.example.com",
			want: `<img src="https://cdn.example.com/a.png"/><img src="//cdn.example.com/b.png"/><a href="https://x.com">x</a>`,
		},
		{
			name: "anchors and mailto untouched",
			in:   `<a href="#sec">s</a><a href="mailto:x@y.com">m</a>`,
			base: "https://blog.example.com",
		},
		{
			name: "video src under files absolutized",
			in:   `<video src="/files/a.mp4" controls></video>`,
			base: "https://blog.example.com",
			want: `<video src="https://blog.example.com/files/a.mp4" controls=""></video>`,
		},
		{
			name: "empty base returns input unchanged",
			in:   `<img src="/files/a.png">`,
			base: "",
		},
		{
			name: "blank input",
			in:   "  ",
			base: "https://blog.example.com",
		},
		{
			name: "no root-relative attribute values",
			in:   `<p>hello <b>world</b></p>`,
			base: "https://blog.example.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AbsolutizeURLs(tt.in, tt.base)
			if tt.want == "" {
				if got != tt.in {
					t.Errorf("AbsolutizeURLs(%q, %q) = %q, want input unchanged", tt.in, tt.base, got)
				}
				return
			}
			if got != tt.want {
				t.Errorf("AbsolutizeURLs(%q, %q) = %q, want %q", tt.in, tt.base, got, tt.want)
			}
		})
	}
}

// AbsolutizeURLs never rewrites a "/" outside an attribute (text content),
// and single-quoted attribute values are rewritten too.
func TestAbsolutizeURLsLeavesTextAlone(t *testing.T) {
	in := `<p>a / b</p>`
	if got := AbsolutizeURLs(in, "https://blog.example.com"); got != in {
		t.Errorf("text slash rewritten: %q", got)
	}
	if got := AbsolutizeURLs(`<img src='/files/a.png'>`, "https://blog.example.com"); !strings.Contains(got, `src="https://blog.example.com/files/a.png"`) {
		t.Errorf("single-quoted src not absolutized: %q", got)
	}
}
