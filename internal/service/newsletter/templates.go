package newsletter

import (
	"bytes"
	"embed"
	"html/template"
	"regexp"
	"strings"
	texttemplate "text/template"
)

// The mail bodies live in emails/ as html+text pairs, one per Rails mailer
// view (article_email, confirmation_email, reply_notification). Html parts
// use html/template like the ERB originals; text parts use text/template so
// URLs and content are not entity-escaped in text/plain bodies.

//go:embed emails
var emailsFS embed.FS

var (
	emailHTMLTemplates = template.Must(template.New("emails").Funcs(template.FuncMap{
		"simpleFormat":     func(s string) template.HTML { return simpleFormat(s, "p") },
		"simpleFormatSpan": func(s string) template.HTML { return simpleFormat(s, "span") },
	}).ParseFS(emailsFS, "emails/*.html"))
	emailTextTemplates = texttemplate.Must(texttemplate.New("emails").ParseFS(emailsFS, "emails/*.text"))
)

// ArticleEmailData feeds the article_email templates
// (NewsletterMailer#article_email). ContentHTML is sanitized at write time
// (plan section 4.4); ContentText and the source fields are plain text
// (full_sanitizer output in Rails). The Go schema has no newsletter footer
// column, so the Footer fields stay empty until one exists; the template
// blocks are kept for parity with the Rails views.
type ArticleEmailData struct {
	Title          string
	Description    string
	HasSource      bool
	SourceAuthor   string
	SourceContent  string
	SourceURL      string
	ContentHTML    template.HTML
	ContentText    string
	ArticleURL     string
	UnsubscribeURL string
	FooterHTML     template.HTML
	FooterText     string
}

// ConfirmationEmailData feeds the confirmation_email templates
// (NewsletterMailer#confirmation_email).
type ConfirmationEmailData struct {
	SiteTitle       string
	ConfirmationURL string
}

// ReplyNotificationEmailData feeds the reply_notification templates
// (CommentMailer#reply_notification). ParentContent is truncated to 120
// characters inside RenderReplyNotificationEmail, like the Rails view.
type ReplyNotificationEmailData struct {
	ReplyAuthor      string
	ReplyContent     string
	ParentContent    string
	CommentableTitle string
	CommentableURL   string
	SiteTitle        string
}

// RenderArticleEmail renders the newsletter article email (html + text).
func RenderArticleEmail(d ArticleEmailData) (htmlBody, textBody string, err error) {
	if htmlBody, err = renderEmailHTML("article_email.html", d); err != nil {
		return "", "", err
	}
	textBody, err = renderEmailText("article_email.text", d)
	return htmlBody, textBody, err
}

// RenderConfirmationEmail renders the subscription confirmation email.
func RenderConfirmationEmail(d ConfirmationEmailData) (htmlBody, textBody string, err error) {
	if htmlBody, err = renderEmailHTML("confirmation_email.html", d); err != nil {
		return "", "", err
	}
	textBody, err = renderEmailText("confirmation_email.text", d)
	return htmlBody, textBody, err
}

// RenderReplyNotificationEmail renders the comment reply notification email.
func RenderReplyNotificationEmail(d ReplyNotificationEmailData) (htmlBody, textBody string, err error) {
	d.ParentContent = truncate(d.ParentContent, 120)
	if htmlBody, err = renderEmailHTML("reply_notification.html", d); err != nil {
		return "", "", err
	}
	textBody, err = renderEmailText("reply_notification.text", d)
	return htmlBody, textBody, err
}

func renderEmailHTML(name string, data any) (string, error) {
	var buf bytes.Buffer
	err := emailHTMLTemplates.ExecuteTemplate(&buf, name, data)
	return buf.String(), err
}

func renderEmailText(name string, data any) (string, error) {
	var buf bytes.Buffer
	err := emailTextTemplates.ExecuteTemplate(&buf, name, data)
	return buf.String(), err
}

// paragraphSplit marks paragraph boundaries for simpleFormat.
var paragraphSplit = regexp.MustCompile(`\r?\n(\s*\r?\n)+`)

// simpleFormat mirrors Rails' simple_format: paragraphs split on blank lines
// are wrapped in tag, single newlines become <br>. Input is escaped first,
// like the Rails helper.
func simpleFormat(s, tag string) template.HTML {
	paras := paragraphSplit.Split(strings.TrimSpace(s), -1)
	for i, p := range paras {
		paras[i] = "<" + tag + ">" + strings.ReplaceAll(template.HTMLEscapeString(p), "\n", "<br>\n") + "</" + tag + ">"
	}
	return template.HTML(strings.Join(paras, "\n\n"))
}

// truncate mirrors String#truncate with the default "..." omission: at most
// length characters (runes) including the omission.
func truncate(s string, length int) string {
	runes := []rune(s)
	if len(runes) <= length {
		return s
	}
	return string(runes[:length-3]) + "..."
}
