// Package captcha implements the server-signed math challenge that replaces
// MathCaptchaHelper + MathCaptchaVerification (plan section 4.5). The
// HMAC-signed token carries the challenge itself, so cached pages stay
// submittable without any server-side session state.
package captcha

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
)

// TTL mirrors MATH_CAPTCHA_TTL (25h): the token must outlive the longest
// page cache (pages 24h, articles 1h).
const TTL = 25 * time.Hour

// Max is the challenge bound used by the comment and subscription forms
// (math_captcha_valid?(max: 10)).
const Max = 10

// Captcha issues and verifies math challenges signed with an HMAC secret.
type Captcha struct {
	secret []byte
	ttl    time.Duration
}

// New returns a Captcha signing tokens with secret (cfg.HMAC_SECRET) and
// expiring them after ttl (TTL in production; shorter in tests).
func New(secret string, ttl time.Duration) *Captcha {
	return &Captcha{secret: []byte(secret), ttl: ttl}
}

// Challenge is one issued math question (MathCaptchaHelper#math_captcha_challenge).
type Challenge struct {
	A        int
	B        int
	Op       string // "+" or "-"
	Question string // e.g. "3 + 4 ="
}

// payload is the signed token body.
type payload struct {
	A   int    `json:"a"`
	B   int    `json:"b"`
	Op  string `json:"op"`
	Exp int64  `json:"exp"`
}

// Issue generates a challenge and its signed token. Mirrors the Rails
// challenge: for "+" b keeps a+b <= Max; for "-" b <= a.
func (c *Captcha) Issue() (question, token string) {
	ch, token := c.IssueChallenge()
	return ch.Question, token
}

// IssueChallenge is Issue plus the challenge fields, for forms that also
// wire the client-side validator (captcha[a]/captcha[b]/captcha[op]).
func (c *Captcha) IssueChallenge() (Challenge, string) {
	var a, b int
	op := "+"
	if rand.IntN(2) == 0 {
		a = rand.IntN(Max + 1)
		b = rand.IntN(Max - a + 1)
	} else {
		a = rand.IntN(Max + 1)
		b = rand.IntN(a + 1)
		op = "-"
	}
	token := c.sign(payload{A: a, B: b, Op: op, Exp: time.Now().Add(c.ttl).Unix()})
	return Challenge{A: a, B: b, Op: op, Question: fmt.Sprintf("%d %s %d =", a, op, b)}, token
}

// Expected returns the correct answer for a token, or false when the token
// is tampered with, expired, or carries an out-of-bounds challenge
// (MathCaptchaVerification#math_captcha_expected).
func (c *Captcha) Expected(token string) (int, bool) {
	p, ok := c.verify(token)
	if !ok {
		return 0, false
	}
	if p.A < 0 || p.A > Max || p.B < 0 || p.B > Max {
		return 0, false
	}
	var expected int
	switch p.Op {
	case "+":
		expected = p.A + p.B
	case "-":
		expected = p.A - p.B
	default:
		return 0, false
	}
	if expected < 0 || expected > Max {
		return 0, false
	}
	return expected, true
}

// Verify reports whether answer matches the token's challenge
// (MathCaptchaVerification#math_captcha_valid?).
func (c *Captcha) Verify(token, answer string) bool {
	expected, ok := c.Expected(token)
	if !ok {
		return false
	}
	n, err := strconv.Atoi(strings.TrimSpace(answer))
	if err != nil {
		return false
	}
	return n == expected
}

// sign encodes payload as base64url(json).base64url(hmac-sha256).
func (c *Captcha) sign(p payload) string {
	body, _ := json.Marshal(p)
	mac := hmac.New(sha256.New, c.secret)
	mac.Write(body)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(body) + "." + enc.EncodeToString(mac.Sum(nil))
}

// verify checks the signature and expiry and returns the payload.
func (c *Captcha) verify(token string) (payload, bool) {
	var p payload
	body, sig, ok := strings.Cut(token, ".")
	if !ok {
		return p, false
	}
	enc := base64.RawURLEncoding
	raw, err := enc.DecodeString(body)
	if err != nil {
		return p, false
	}
	want, err := enc.DecodeString(sig)
	if err != nil {
		return p, false
	}
	mac := hmac.New(sha256.New, c.secret)
	mac.Write(raw)
	if !hmac.Equal(mac.Sum(nil), want) {
		return p, false
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, false
	}
	if time.Now().Unix() >= p.Exp {
		return p, false
	}
	return p, true
}
