package newsletter

import (
	"bufio"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"rables/internal/db/query"
)

func TestConfigFromSetting(t *testing.T) {
	tests := []struct {
		name string
		in   query.NewsletterSetting
		want SMTPConfig
	}{
		{
			name: "defaults",
			in:   query.NewsletterSetting{FromEmail: sql.NullString{String: "news@example.com", Valid: true}},
			want: SMTPConfig{Port: 587, Domain: "example.com", Authentication: "plain", EnableStartTLS: true, FromEmail: "news@example.com"},
		},
		{
			name: "explicit values",
			in: query.NewsletterSetting{
				FromEmail:          sql.NullString{String: "news@example.com", Valid: true},
				SmtpAddress:        sql.NullString{String: "smtp.example.com", Valid: true},
				SmtpPort:           sql.NullInt64{Int64: 2525, Valid: true},
				SmtpUserName:       sql.NullString{String: "u", Valid: true},
				SmtpPassword:       sql.NullString{String: "p", Valid: true},
				SmtpDomain:         sql.NullString{String: "helo.example.com", Valid: true},
				SmtpAuthentication: sql.NullString{String: "LOGIN", Valid: true},
				SmtpEnableStarttls: sql.NullInt64{Int64: 0, Valid: true},
			},
			want: SMTPConfig{
				Address: "smtp.example.com", Port: 2525, Domain: "helo.example.com",
				UserName: "u", Password: "p", Authentication: "login", EnableStartTLS: false,
				FromEmail: "news@example.com",
			},
		},
		{
			name: "starttls explicitly enabled",
			in:   query.NewsletterSetting{SmtpEnableStarttls: sql.NullInt64{Int64: 1, Valid: true}},
			want: SMTPConfig{Port: 587, Authentication: "plain", EnableStartTLS: true},
		},
		{
			name: "unknown authentication falls back to plain",
			in:   query.NewsletterSetting{SmtpAuthentication: sql.NullString{String: "xoauth2", Valid: true}},
			want: SMTPConfig{Port: 587, Authentication: "plain", EnableStartTLS: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ConfigFromSetting(tt.in); got != tt.want {
				t.Errorf("ConfigFromSetting() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestValidateSetting(t *testing.T) {
	tests := []struct {
		name string
		in   query.NewsletterSetting
		want []string
	}{
		{"disabled passes blank", query.NewsletterSetting{Provider: "native"}, nil},
		{"bad provider", query.NewsletterSetting{Provider: "bogus"}, []string{"Provider is not included in the list"}},
		{
			name: "enabled native without from_email fails both validators",
			in:   query.NewsletterSetting{Enabled: 1, Provider: "native"},
			want: []string{"From email can't be blank", "From email is invalid"},
		},
		{
			name: "enabled native with malformed from_email",
			in:   query.NewsletterSetting{Enabled: 1, Provider: "native", FromEmail: sql.NullString{String: "nope", Valid: true}},
			want: []string{"From email is invalid"},
		},
		{
			name: "enabled native with email",
			in:   query.NewsletterSetting{Enabled: 1, Provider: "native", FromEmail: sql.NullString{String: "a@b.co", Valid: true}},
			want: nil,
		},
		{
			name: "enabled listmonk skips from_email",
			in:   query.NewsletterSetting{Enabled: 1, Provider: "listmonk"},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidateSetting(tt.in)
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("ValidateSetting() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidateListmonk(t *testing.T) {
	tests := []struct {
		name string
		in   query.Listmonk
		want []string
	}{
		{
			name: "all blank",
			in:   query.Listmonk{},
			want: []string{"Api key can't be blank", "Username can't be blank", "Url can't be blank", "Url 格式无效"},
		},
		{
			name: "url without scheme",
			in: query.Listmonk{
				ApiKey:   sql.NullString{String: "k", Valid: true},
				Username: sql.NullString{String: "u", Valid: true},
				Url:      sql.NullString{String: "example.com", Valid: true},
			},
			want: []string{"Url 格式无效"},
		},
		{
			name: "valid",
			in: query.Listmonk{
				ApiKey:   sql.NullString{String: "k", Valid: true},
				Username: sql.NullString{String: "u", Valid: true},
				Url:      sql.NullString{String: "https://example.com", Valid: true},
			},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidateListmonk(tt.in)
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("ValidateListmonk() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRenderArticleEmail(t *testing.T) {
	full := ArticleEmailData{
		Title:          "Hello 世界",
		Description:    "A description",
		HasSource:      true,
		SourceAuthor:   "Author <Name>",
		SourceContent:  "Quoted\ntext",
		SourceURL:      "https://source.example.com/post",
		ContentHTML:    `<p>Body <strong>HTML</strong></p>`,
		ContentText:    "Body HTML",
		ArticleURL:     "https://blog.example.com/articles/hello",
		UnsubscribeURL: "https://blog.example.com/unsubscribe?token=abc",
		FooterHTML:     `<p>Footer</p>`,
		FooterText:     "Footer",
	}
	htmlBody, textBody, err := RenderArticleEmail(full)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	htmlMarkers := []string{
		"<h1>Hello 世界</h1>",
		"<p>A description</p>",
		"source-reference__quote",
		"Author &lt;Name&gt;",
		`href="https://source.example.com/post"`,
		`<p>Body <strong>HTML</strong></p>`,
		`href="https://blog.example.com/articles/hello"`,
		"<p>Footer</p>",
		`href="https://blog.example.com/unsubscribe?token=abc"`,
	}
	for _, m := range htmlMarkers {
		if !strings.Contains(htmlBody, m) {
			t.Errorf("html body missing %q", m)
		}
	}
	textMarkers := []string{
		"Hello 世界", "A description", "来源: Author <Name>", "Quoted\ntext",
		"原文: https://source.example.com/post", "Body HTML",
		"阅读全文: https://blog.example.com/articles/hello", "Footer",
		"取消订阅: https://blog.example.com/unsubscribe?token=abc",
	}
	for _, m := range textMarkers {
		if !strings.Contains(textBody, m) {
			t.Errorf("text body missing %q", m)
		}
	}

	// Minimal data drops the optional blocks, like the Rails conditionals.
	htmlBody, textBody, err = RenderArticleEmail(ArticleEmailData{
		Title: "T", ContentHTML: `<p>x</p>`, ContentText: "x",
		ArticleURL: "https://b/a", UnsubscribeURL: "https://b/u",
	})
	if err != nil {
		t.Fatalf("render minimal: %v", err)
	}
	for _, absent := range []string{`<blockquote class="source-reference__quote">`, `<div class="footer">`} {
		if strings.Contains(htmlBody, absent) {
			t.Errorf("minimal html body should not contain %q", absent)
		}
	}
	for _, absent := range []string{"参考:", "原文:", "Footer"} {
		if strings.Contains(textBody, absent) {
			t.Errorf("minimal text body should not contain %q", absent)
		}
	}
}

func TestRenderConfirmationEmail(t *testing.T) {
	htmlBody, textBody, err := RenderConfirmationEmail(ConfirmationEmailData{
		SiteTitle: "My Blog", ConfirmationURL: "https://blog.example.com/confirm?token=x",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(htmlBody, `href="https://blog.example.com/confirm?token=x"`) ||
		!strings.Contains(htmlBody, "感谢您订阅 My Blog 的邮件列表！") {
		t.Error("html body missing confirmation link or site title")
	}
	if !strings.Contains(textBody, "https://blog.example.com/confirm?token=x") ||
		!strings.Contains(textBody, "确认您的订阅") {
		t.Error("text body missing confirmation url")
	}
}

func TestRenderReplyNotificationEmail(t *testing.T) {
	longParent := strings.Repeat("字", 200)
	htmlBody, textBody, err := RenderReplyNotificationEmail(ReplyNotificationEmailData{
		ReplyAuthor:      "Bob",
		ReplyContent:     "first\n\nsecond <b>",
		ParentContent:    longParent,
		CommentableTitle: "Post",
		CommentableURL:   "https://blog.example.com/articles/post",
		SiteTitle:        "My Blog",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// simple_format: paragraphs and <br>, escaped input.
	if !strings.Contains(htmlBody, "<p>first</p>") || !strings.Contains(htmlBody, "<p>second &lt;b&gt;</p>") {
		t.Error("html body missing simple_format paragraphs")
	}
	// truncate(parent, 120): 117 runes plus the "..." omission.
	wantParent := strings.Repeat("字", 117) + "..."
	if !strings.Contains(textBody, wantParent) {
		t.Error("text body missing truncated parent comment")
	}
	if strings.Contains(textBody, strings.Repeat("字", 118)) {
		t.Error("parent comment was not truncated to 120 runes")
	}
	for _, m := range []string{"Bob", "Post", "https://blog.example.com/articles/post", "My Blog"} {
		if !strings.Contains(textBody, m) {
			t.Errorf("text body missing %q", m)
		}
	}
}

// fakeSMTPServer is a minimal SMTP server for handshake and delivery tests
// (plan T18 DoD: verify against a fake SMTP server over net.Listen).
type fakeSMTPServer struct {
	addr string
	ln   net.Listener

	mu       sync.Mutex
	failAuth bool
	auths    [][2]string // captured user/password pairs
	mails    []string    // raw DATA payloads
	quit     bool
}

func newFakeSMTPServer(t *testing.T, failAuth bool) *fakeSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &fakeSMTPServer{addr: ln.Addr().String(), ln: ln, failAuth: failAuth}
	go srv.serve()
	t.Cleanup(func() { srv.ln.Close() })
	return srv
}

func (s *fakeSMTPServer) port() int {
	_, port, _ := net.SplitHostPort(s.addr)
	var p int
	fmt.Sscanf(port, "%d", &p)
	return p
}

func (s *fakeSMTPServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeSMTPServer) handle(conn net.Conn) {
	defer conn.Close()
	w := bufio.NewWriter(conn)
	r := bufio.NewReader(conn)
	reply := func(format string, args ...any) {
		fmt.Fprintf(w, format+"\r\n", args...)
		w.Flush()
	}
	reply("220 fake ESMTP ready")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			// No STARTTLS advertised: the verify tests exercise the
			// not-advertised branch, delivery runs opportunistic plaintext.
			reply("250-fake\r\n250-AUTH PLAIN LOGIN CRAM-MD5\r\n250 8BITMIME")
		case "AUTH":
			s.handleAuth(w, r, arg)
		case "MAIL", "RCPT", "NOOP", "RSET":
			reply("250 OK")
		case "DATA":
			reply("354 End data with <CR><LF>.<CR><LF>")
			var payload strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(l, "\r\n") == "." {
					break
				}
				payload.WriteString(l)
			}
			s.mu.Lock()
			s.mails = append(s.mails, payload.String())
			s.mu.Unlock()
			reply("250 Queued")
		case "QUIT":
			reply("221 Bye")
			return
		default:
			reply("502 Command not implemented")
		}
	}
}

func (s *fakeSMTPServer) handleAuth(w *bufio.Writer, r *bufio.Reader, arg string) {
	mech, rest, _ := strings.Cut(arg, " ")
	finish := func(user, pass string) {
		s.mu.Lock()
		s.auths = append(s.auths, [2]string{user, pass})
		s.mu.Unlock()
		if s.failAuth {
			fmt.Fprint(w, "535 5.7.8 authentication failed\r\n")
		} else {
			fmt.Fprint(w, "235 2.7.0 authentication succeeded\r\n")
		}
		w.Flush()
	}
	readLine := func() string {
		l, _ := r.ReadString('\n')
		return strings.TrimRight(l, "\r\n")
	}
	decode := func(v string) string {
		b, _ := base64.StdEncoding.DecodeString(v)
		return string(b)
	}
	switch strings.ToUpper(mech) {
	case "PLAIN":
		// Initial response may be inline (AUTH PLAIN <b64>) or on the next line.
		initial := rest
		if initial == "" {
			fmt.Fprint(w, "334 \r\n")
			w.Flush()
			initial = readLine()
		}
		parts := strings.Split(decode(initial), "\x00")
		user, pass := "", ""
		if len(parts) == 3 {
			user, pass = parts[1], parts[2]
		}
		finish(user, pass)
	case "LOGIN":
		fmt.Fprint(w, "334 VXNlcm5hbWU6\r\n")
		w.Flush()
		user := decode(readLine())
		fmt.Fprint(w, "334 UGFzc3dvcmQ6\r\n")
		w.Flush()
		pass := decode(readLine())
		finish(user, pass)
	case "CRAM-MD5":
		challenge := base64.StdEncoding.EncodeToString([]byte("<12345@fake>"))
		fmt.Fprintf(w, "334 %s\r\n", challenge)
		w.Flush()
		fields := strings.Fields(decode(readLine()))
		user := ""
		if len(fields) > 0 {
			user = fields[0]
		}
		finish(user, "")
	default:
		fmt.Fprint(w, "504 Unrecognized authentication type\r\n")
		w.Flush()
	}
}

func verifyConfig(addr string, port int) SMTPConfig {
	host, _, _ := net.SplitHostPort(addr)
	return SMTPConfig{
		Address: host, Port: port, Domain: "example.com",
		UserName: "user", Password: "secret",
		Authentication: "plain", EnableStartTLS: false,
		FromEmail: "news@example.com",
	}
}

func TestVerifySMTPSuccess(t *testing.T) {
	srv := newFakeSMTPServer(t, false)
	if err := VerifySMTP(t.Context(), verifyConfig(srv.addr, srv.port())); err != nil {
		t.Fatalf("VerifySMTP: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.auths) != 1 || srv.auths[0] != [2]string{"user", "secret"} {
		t.Errorf("captured auths = %v, want one user/secret", srv.auths)
	}
}

func TestVerifySMTPLoginAuth(t *testing.T) {
	srv := newFakeSMTPServer(t, false)
	cfg := verifyConfig(srv.addr, srv.port())
	cfg.Authentication = "login"
	if err := VerifySMTP(t.Context(), cfg); err != nil {
		t.Fatalf("VerifySMTP login: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.auths) != 1 || srv.auths[0] != [2]string{"user", "secret"} {
		t.Errorf("captured auths = %v, want one user/secret", srv.auths)
	}
}

func TestVerifySMTPAuthFailure(t *testing.T) {
	srv := newFakeSMTPServer(t, true)
	err := VerifySMTP(t.Context(), verifyConfig(srv.addr, srv.port()))
	if !errors.Is(err, ErrAuthentication) {
		t.Errorf("VerifySMTP error = %v, want ErrAuthentication", err)
	}
}

func TestVerifySMTPStartTLSNotAdvertised(t *testing.T) {
	srv := newFakeSMTPServer(t, false)
	cfg := verifyConfig(srv.addr, srv.port())
	cfg.EnableStartTLS = true // mandatory in verify, like Net::SMTP#enable_starttls
	err := VerifySMTP(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("VerifySMTP error = %v, want a STARTTLS complaint", err)
	}
}

func TestVerifySMTPConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens now
	cfg := verifyConfig(addr, 0)
	_, port, _ := net.SplitHostPort(addr)
	fmt.Sscanf(port, "%d", &cfg.Port)
	err = VerifySMTP(t.Context(), cfg)
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Errorf("VerifySMTP error = %v, want a net.Error", err)
	}
}

func TestMailerSend(t *testing.T) {
	srv := newFakeSMTPServer(t, false)
	cfg := verifyConfig(srv.addr, srv.port())
	cfg.EnableStartTLS = true // opportunistic in Send: fake lacks STARTTLS, falls back to plaintext
	mailer := NewMailer(cfg)
	err := mailer.Send(t.Context(), Message{
		To:      "sub@example.com",
		Subject: "Hello | My Blog",
		Text:    "plain body",
		HTML:    "<p>html body</p>",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.mails) != 1 {
		t.Fatalf("received %d mails, want 1", len(srv.mails))
	}
	mail := srv.mails[0]
	for _, m := range []string{
		"From: <news@example.com>", "To: <sub@example.com>", "Subject: Hello | My Blog",
		"text/plain", "plain body", "text/html", "<p>html body</p>",
	} {
		if !strings.Contains(mail, m) {
			t.Errorf("sent mail missing %q", m)
		}
	}
}

func TestListmonkFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, key, ok := r.BasicAuth()
		if !ok || user != "admin" || key != "token" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"Invalid API credentials"}`)
			return
		}
		switch r.URL.Path {
		case "/api/lists":
			fmt.Fprint(w, `{"data":{"results":[{"id":3,"name":"Newsletter"}],"total":1}}`)
		case "/api/templates":
			fmt.Fprint(w, `{"data":[{"id":7,"name":"Default"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := ListmonkClient{URL: srv.URL, Username: "admin", APIKey: "token"}
	lists, err := client.FetchLists(t.Context())
	if err != nil {
		t.Fatalf("FetchLists: %v", err)
	}
	if len(lists) != 1 || lists[0].ID != 3 || lists[0].Name != "Newsletter" {
		t.Errorf("lists = %+v", lists)
	}
	tmpls, err := client.FetchTemplates(t.Context())
	if err != nil {
		t.Fatalf("FetchTemplates: %v", err)
	}
	if len(tmpls) != 1 || tmpls[0].ID != 7 || tmpls[0].Name != "Default" {
		t.Errorf("templates = %+v", tmpls)
	}

	bad := ListmonkClient{URL: srv.URL, Username: "admin", APIKey: "wrong"}
	if _, err := bad.FetchLists(t.Context()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("FetchLists error = %v, want 401 detail", err)
	}
	if _, err := bad.FetchTemplates(t.Context()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("FetchTemplates error = %v, want 401 detail", err)
	}
}
