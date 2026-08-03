package httpd

import (
	"encoding/base64"
	"encoding/json"
	"net/http"

	"rables/internal/templates"
)

// flashCookieName carries the one-time flash between a redirect and the next
// render, replacing the Rails session-backed flash.
const flashCookieName = "flash"

// SetFlash stores the flash in an HttpOnly SameSite=Lax cookie so the next
// request can render it once.
func SetFlash(w http.ResponseWriter, flash templates.Flash) {
	payload, err := json.Marshal(flash)
	if err != nil {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookieName,
		Value:    base64.RawURLEncoding.EncodeToString(payload),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// PopFlash reads the flash cookie and clears it, so it is shown exactly once.
// Missing or malformed cookies yield a zero Flash.
func PopFlash(r *http.Request, w http.ResponseWriter) templates.Flash {
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
	})
	var flash templates.Flash
	payload, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		return templates.Flash{}
	}
	if err := json.Unmarshal(payload, &flash); err != nil {
		return templates.Flash{}
	}
	return flash
}
