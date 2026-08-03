package newsletter

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/wneessen/go-mail/smtp"

	"rables/internal/db/query"
)

// ErrAuthentication marks handshake failures that Rails reports as
// "Authentication failed: Invalid credentials." (Net::SMTPAuthenticationError
// plus Net::SMTPFatalError, i.e. any 5xx reply during start).
var ErrAuthentication = errors.New("newsletter: smtp authentication failed")

// VerifySMTP mirrors Admin::NewsletterController#verify_smtp: dial, EHLO,
// STARTTLS (mandatory when enabled, like Net::SMTP#enable_starttls) and AUTH
// when credentials are configured, stopping before RCPT.
func VerifySMTP(ctx context.Context, cfg SMTPConfig) error {
	dialer := net.Dialer{Timeout: 5 * time.Second} // Rails open_timeout = 5
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(cfg.Address, strconv.Itoa(cfg.Port)))
	if err != nil {
		return err
	}
	defer conn.Close()
	// Rails read_timeout = 5 applies per read; a single overall deadline keeps
	// a silently-stalling fake server from hanging the admin request.
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

	client, err := smtp.NewClient(conn, cfg.Address)
	if err != nil {
		return err
	}
	if cfg.Domain != "" {
		err = client.Hello(cfg.Domain)
	} else {
		err = client.Hello("localhost")
	}
	if err != nil {
		return classifySMTPError(err)
	}
	if cfg.EnableStartTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			// Net::SMTP#enable_starttls raises Net::SMTPUnsupportedCommand, a
			// Net::SMTPError subclass; the 4xx code keeps it out of
			// ErrAuthentication so the handler reports it as "SMTP error:".
			return &textproto.Error{Code: 454, Msg: "STARTTLS is not supported on this server"}
		}
		if err := client.StartTLS(&tls.Config{ServerName: cfg.Address, MinVersion: tls.VersionTLS12}); err != nil {
			return classifySMTPError(err)
		}
	}
	if cfg.UserName != "" {
		if err := client.Auth(smtpAuth(cfg)); err != nil {
			return classifySMTPError(err)
		}
	}
	_ = client.Quit()
	return nil
}

// smtpAuth builds the smtp.Auth for the configured mechanism. Unencrypted
// connections are allowed, matching Net::SMTP (Rails never refuses to
// authenticate on a plain connection when STARTTLS is disabled).
func smtpAuth(cfg SMTPConfig) smtp.Auth {
	switch cfg.Authentication {
	case "login":
		return smtp.LoginAuth(cfg.UserName, cfg.Password, cfg.Address, true)
	case "cram_md5":
		return smtp.CRAMMD5Auth(cfg.UserName, cfg.Password)
	default:
		return smtp.PlainAuth("", cfg.UserName, cfg.Password, cfg.Address, true)
	}
}

// classifySMTPError wraps 5xx protocol replies as ErrAuthentication, like the
// shared rescue of Net::SMTPAuthenticationError and Net::SMTPFatalError.
func classifySMTPError(err error) error {
	var tpErr *textproto.Error
	if errors.As(err, &tpErr) && tpErr.Code >= 500 {
		return fmt.Errorf("%w: %s", ErrAuthentication, tpErr.Msg)
	}
	return err
}

// ListmonkClient is a basic-auth client for the listmonk API subset used by
// the admin page (app/models/listmonk.rb).
type ListmonkClient struct {
	URL      string
	Username string
	APIKey   string
	// HTTPClient overrides the default client (5s dial / 10s read timeouts,
	// Listmonk::HTTP_OPEN_TIMEOUT / HTTP_READ_TIMEOUT); used by tests.
	HTTPClient *http.Client
}

// Configured mirrors Listmonk#configured?.
func (c ListmonkClient) Configured() bool {
	return c.APIKey != "" && c.Username != "" && c.URL != ""
}

// ValidateListmonk mirrors the Listmonk model validations, returning
// Rails-style full error messages in declaration order.
func ValidateListmonk(lm query.Listmonk) []string {
	var errs []string
	if strings.TrimSpace(lm.ApiKey.String) == "" {
		errs = append(errs, "Api key can't be blank")
	}
	if strings.TrimSpace(lm.Username.String) == "" {
		errs = append(errs, "Username can't be blank")
	}
	if strings.TrimSpace(lm.Url.String) == "" {
		errs = append(errs, "Url can't be blank")
	}
	// The format validator runs even on a blank value in Rails.
	if u, err := url.Parse(lm.Url.String); err != nil || u.Scheme == "" {
		errs = append(errs, "Url 格式无效")
	}
	return errs
}

// ListmonkList is one entry of the /api/lists results array.
type ListmonkList struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// ListmonkTemplate is one entry of the /api/templates data array.
type ListmonkTemplate struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// FetchLists mirrors Listmonk#fetch_lists: GET {url}/api/lists with basic
// auth, returning data.results.
func (c ListmonkClient) FetchLists(ctx context.Context) ([]ListmonkList, error) {
	var body struct {
		Data struct {
			Results []ListmonkList `json:"results"`
		} `json:"data"`
	}
	if err := c.get(ctx, c.URL+"/api/lists", &body); err != nil {
		return nil, fmt.Errorf("Fetch Lists failed! %w", err)
	}
	return body.Data.Results, nil
}

// FetchTemplates mirrors Listmonk#fetch_templates: GET {url}/api/templates
// with basic auth, returning data.
func (c ListmonkClient) FetchTemplates(ctx context.Context) ([]ListmonkTemplate, error) {
	var body struct {
		Data []ListmonkTemplate `json:"data"`
	}
	if err := c.get(ctx, c.URL+"/api/templates", &body); err != nil {
		return nil, fmt.Errorf("Fetch Template failed! %w", err)
	}
	return body.Data, nil
}

// get performs one authenticated GET and decodes the JSON body into out.
// Non-2xx responses carry the "CODE - BODY" text of the Rails errors.
func (c ListmonkClient) get(ctx context.Context, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.Username, c.APIKey)

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 15 * time.Second, // whole request, including the body read
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				ResponseHeaderTimeout: 10 * time.Second,
			},
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var buf [512]byte
		n, _ := resp.Body.Read(buf[:])
		return fmt.Errorf("%d - %s", resp.StatusCode, string(buf[:n]))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
