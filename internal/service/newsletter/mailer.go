// Package newsletter is the email infrastructure of the Go rewrite
// (plan T18): an SMTP mailer configured from the newsletter_settings
// singleton row (mirroring SmtpConfigurable / ActionMailer per-mail
// delivery), connection verification for the admin page
// (Admin::NewsletterController#verify), a basic-auth listmonk API client
// (app/models/listmonk.rb) and the html+text bodies of the outgoing mail
// types (NewsletterMailer, CommentMailer views).
package newsletter

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	mail "github.com/wneessen/go-mail"

	"rables/internal/db/query"
)

// SMTPConfig is the resolved SMTP connection configuration, mirroring
// SmtpConfigurable#prepare_smtp_config.
type SMTPConfig struct {
	Address        string
	Port           int    // defaults to 587
	Domain         string // HELO domain; defaults to the FromEmail host part
	UserName       string
	Password       string
	Authentication string // plain | login | cram_md5 (default plain)
	EnableStartTLS bool   // Rails smtp_enable_starttls != false (nil counts as true)
	FromEmail      string
}

// ConfigFromSetting maps the newsletter_settings row to an SMTPConfig.
func ConfigFromSetting(st query.NewsletterSetting) SMTPConfig {
	from := st.FromEmail.String
	domain := st.SmtpDomain.String
	if domain == "" {
		if i := strings.LastIndex(from, "@"); i >= 0 {
			domain = from[i+1:]
		}
	}
	auth := strings.ToLower(st.SmtpAuthentication.String)
	switch auth {
	case "plain", "login", "cram_md5":
	default:
		auth = "plain"
	}
	port := 587
	if st.SmtpPort.Valid && st.SmtpPort.Int64 > 0 {
		port = int(st.SmtpPort.Int64)
	}
	// Rails: newsletter_setting.smtp_enable_starttls != false, so a NULL
	// column (never explicitly disabled) enables STARTTLS.
	starttls := true
	if st.SmtpEnableStarttls.Valid && st.SmtpEnableStarttls.Int64 == 0 {
		starttls = false
	}
	return SMTPConfig{
		Address:        st.SmtpAddress.String,
		Port:           port,
		Domain:         domain,
		UserName:       st.SmtpUserName.String,
		Password:       st.SmtpPassword.String,
		Authentication: auth,
		EnableStartTLS: starttls,
		FromEmail:      from,
	}
}

// emailRE approximates URI::MailTo::EMAIL_REGEXP (simplified, same as the
// comment author email check).
var emailRE = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// ValidateSetting mirrors the NewsletterSetting model validations, returning
// Rails-style full error messages in declaration order.
func ValidateSetting(st query.NewsletterSetting) []string {
	var errs []string
	if st.Provider != "native" && st.Provider != "listmonk" {
		errs = append(errs, "Provider is not included in the list")
	}
	if st.Enabled != 0 && st.Provider == "native" {
		// Blank fails both the presence and the format validator in Rails.
		if strings.TrimSpace(st.FromEmail.String) == "" {
			errs = append(errs, "From email can't be blank")
		}
		if !emailRE.MatchString(st.FromEmail.String) {
			errs = append(errs, "From email is invalid")
		}
	}
	return errs
}

// Message is one outgoing email with both html and text bodies, matching the
// dual mailer views of the Rails app.
type Message struct {
	To      string
	From    string // empty falls back to the mailer's FromEmail
	Subject string
	HTML    string
	Text    string
}

// Mailer sends messages over SMTP using a snapshot SMTPConfig. Build one per
// send so config changes apply to the next mail, like the Rails lambda
// default / per-mail delivery_method.
type Mailer struct {
	cfg SMTPConfig
}

// NewMailer returns a Mailer for cfg.
func NewMailer(cfg SMTPConfig) *Mailer {
	return &Mailer{cfg: cfg}
}

// Send delivers msg, mirroring ActionMailer's smtp delivery with the
// per-mail settings of SmtpConfigurable: STARTTLS is opportunistic
// (enable_starttls_auto) and AUTH runs only when a username is configured.
func (m *Mailer) Send(ctx context.Context, msg Message) error {
	from := msg.From
	if from == "" {
		from = m.cfg.FromEmail
	}
	gmsg := mail.NewMsg()
	if err := gmsg.From(from); err != nil {
		return fmt.Errorf("newsletter: from: %w", err)
	}
	if err := gmsg.To(msg.To); err != nil {
		return fmt.Errorf("newsletter: to: %w", err)
	}
	gmsg.Subject(msg.Subject)
	// Text body first, html as the alternative, like the ActionMailer
	// multipart/alternative part order.
	gmsg.SetBodyString(mail.TypeTextPlain, msg.Text)
	if msg.HTML != "" {
		gmsg.AddAlternativeString(mail.TypeTextHTML, msg.HTML)
	}

	client, err := m.client()
	if err != nil {
		return err
	}
	return client.DialAndSendWithContext(ctx, gmsg)
}

// client builds the go-mail client for the snapshot config.
func (m *Mailer) client() (*mail.Client, error) {
	policy := mail.NoTLS
	if m.cfg.EnableStartTLS {
		policy = mail.TLSOpportunistic
	}
	opts := []mail.Option{
		mail.WithPort(m.cfg.Port),
		mail.WithTLSPolicy(policy),
		mail.WithTimeout(15 * time.Second),
	}
	if m.cfg.Domain != "" {
		opts = append(opts, mail.WithHELO(m.cfg.Domain))
	}
	if m.cfg.UserName != "" {
		opts = append(opts,
			mail.WithSMTPAuth(smtpAuthType(m.cfg.Authentication)),
			mail.WithUsername(m.cfg.UserName),
			mail.WithPassword(m.cfg.Password),
		)
	}
	return mail.NewClient(m.cfg.Address, opts...)
}

// smtpAuthType maps the Rails authentication string to go-mail.
func smtpAuthType(authentication string) mail.SMTPAuthType {
	switch strings.ToLower(authentication) {
	case "login":
		return mail.SMTPAuthLogin
	case "cram_md5":
		return mail.SMTPAuthCramMD5
	default:
		return mail.SMTPAuthPlain
	}
}
