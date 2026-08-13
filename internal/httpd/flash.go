package httpd

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"rables/internal/templates"
)

// flashCookieName carries the one-time flash between a redirect and the next
// render, replacing the Rails session-backed flash.
const flashCookieName = "flash"

// SetFlash stores the flash in an HttpOnly SameSite=Lax cookie so the next
// request can render it once. The value is HMAC-signed like the math-captcha
// tokens (base64url(json).base64url(mac)) so a client cannot plant arbitrary
// flash content (cookie tossing, plain-HTTP MitM).
func (s *Server) SetFlash(w http.ResponseWriter, flash templates.Flash) {
	payload, err := json.Marshal(flash)
	if err != nil {
		return
	}
	mac := hmac.New(sha256.New, []byte(s.Cfg.HMACSecret))
	mac.Write(payload)
	enc := base64.RawURLEncoding
	value := enc.EncodeToString(payload) + "." + enc.EncodeToString(mac.Sum(nil))
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.Cfg.SecureCookies,
	})
}

// redirectWithFlash stores the flash and redirects to target. The target
// gets a cache-busting "_" query parameter (a short-encoded timestamp):
// cacheable public pages (index, article, page) are served with max-age
// cache headers when no flash cookie is present, so a POST→302 back to a
// page the browser still holds fresh would be answered from the cache — the
// request never reaches the server, the flash cookie is never popped, and
// the message surfaces on some later, unrelated page (or the user resubmits,
// thinking the first attempt failed). A unique query string forces a cache
// miss, so the fresh request carries the flash cookie into the no-cache
// branch and the flash renders once, where it belongs.
func (s *Server) redirectWithFlash(w http.ResponseWriter, r *http.Request, target string, flash templates.Flash) {
	s.SetFlash(w, flash)
	http.Redirect(w, r, cacheBustTarget(target), http.StatusFound)
}

// cacheBustTarget appends the throwaway "_" query parameter described on
// redirectWithFlash, using "&" when the target already carries a query.
func cacheBustTarget(target string) string {
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	return target + sep + "_=" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// PopFlash reads the flash cookie and clears it, so it is shown exactly once.
// Missing, malformed, or bad-signature cookies yield a zero Flash.
func (s *Server) PopFlash(r *http.Request, w http.ResponseWriter) templates.Flash {
	cookie, err := r.Cookie(flashCookieName)
	if err != nil {
		return templates.Flash{}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.Cfg.SecureCookies,
	})
	body, sig, ok := strings.Cut(cookie.Value, ".")
	if !ok {
		return templates.Flash{}
	}
	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(body)
	if err != nil {
		return templates.Flash{}
	}
	want, err := enc.DecodeString(sig)
	if err != nil {
		return templates.Flash{}
	}
	mac := hmac.New(sha256.New, []byte(s.Cfg.HMACSecret))
	mac.Write(payload)
	if !hmac.Equal(mac.Sum(nil), want) {
		return templates.Flash{}
	}
	var flash templates.Flash
	if err := json.Unmarshal(payload, &flash); err != nil {
		return templates.Flash{}
	}
	return flash
}

// flashErrorLimit caps the batch-operation errors joined into the flash
// cookie: a full page of failures would produce a Set-Cookie header past
// browser (4KB) and proxy buffer limits, silently dropping the flash or
// 502ing the response. The full list stays in the activity log.
const flashErrorLimit = 10

// flashErrorEntryRunes caps each error entry joined into the flash: entries
// embed user-controlled labels, so without it one giant entry would overflow
// cookie/proxy limits even within flashErrorLimit.
const flashErrorEntryRunes = 200

// flashMaxBytes caps the joined flash text in bytes: the rune-based caps
// above still let 2000 CJK runes (~6KB UTF-8) through, and after JSON
// escaping plus base64's 4/3 growth that overflows the same 4KB cookie/proxy
// limits. ~1.5KB leaves headroom for that expansion.
const flashMaxBytes = 1500

// joinFlashErrors joins batch-operation errors for a flash message,
// truncated to flashErrorLimit entries, each capped at flashErrorEntryRunes
// runes, with the joined result capped at flashMaxBytes bytes (see the
// constants).
func joinFlashErrors(errs []string) string {
	truncated := make([]string, 0, min(len(errs), flashErrorLimit))
	for _, err := range errs[:min(len(errs), flashErrorLimit)] {
		truncated = append(truncated, truncateRunes(err, flashErrorEntryRunes))
	}
	joined := truncateBytes(strings.Join(truncated, "; "), flashMaxBytes)
	if len(errs) <= flashErrorLimit {
		return joined
	}
	return joined + fmt.Sprintf("; …以及其余 %d 条", len(errs)-flashErrorLimit)
}

// truncateBytes caps s at n bytes without splitting a multi-byte UTF-8
// character, appending an ellipsis when truncating (like truncateRunes).
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	n -= 3
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}
