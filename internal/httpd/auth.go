package httpd

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"rables/internal/db/query"
	"rables/internal/templates"
)

// bcryptCost matches the cost used when creating password digests. Digests
// produced by the Rails app (has_secure_password / bcrypt gem) verify with
// any cost, so existing digests stay compatible.
const bcryptCost = 10

// sessionTTL bounds how long a session stays valid: RequireAuth rejects (and
// deletes) sessions older than this, and the login cookie's MaxAge matches so
// the browser drops it at the same time. The scheduler's deleteExpiredSessions
// task sweeps rows that never get visited again.
const sessionTTL = 30 * 24 * time.Hour

// dummyPasswordDigest stands in for the user digest when the login username
// does not exist: the login handler still runs one bcrypt comparison against
// it, so the response time does not reveal whether the username was valid.
// Cost 12 matches the Rails-migrated digests (has_secure_password's default),
// so the dummy comparison takes the same time as one against a real digest.
// Generated once at package init.
var dummyPasswordDigest = func() []byte {
	digest, err := bcrypt.GenerateFromPassword([]byte("dummy-password"), 12)
	if err != nil {
		panic(err)
	}
	return digest
}()

// loginPageData feeds auth_login.html.
type loginPageData struct {
	Flash    templates.Flash
	UserName string
}

// loginForm renders GET /session/new.
func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "auth_login", loginPageData{
		Flash:    s.PopFlash(r, w),
		UserName: r.URL.Query().Get("user_name"),
	})
}

// login handles POST /session, mirroring SessionsController#create.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !parseCappedForm(w, r) {
		return
	}
	userName := normalizeUserName(r.FormValue("user_name"))
	password := r.FormValue("password")

	user, err := s.Q.GetUserByUserName(r.Context(), userName)
	// Compare against a dummy digest when the user is missing so unknown
	// usernames cost the same bcrypt time as wrong passwords (no user
	// enumeration via timing). Any lookup error lands here too and reports
	// as a plain bad-credentials failure, as before.
	digest := dummyPasswordDigest
	if err == nil {
		digest = []byte(user.PasswordDigest)
	}
	match := bcrypt.CompareHashAndPassword(digest, []byte(password)) == nil
	// A failed lookup must never authenticate, even when the comparison
	// matches (the password literally equals the dummy's): a zero-value
	// user would fall through to startSession and 500 on the sessions FK.
	if err != nil || !match {
		s.SetFlash(w, templates.Flash{Alert: "Try another username or password."})
		http.Redirect(w, r, "/session/new", http.StatusFound)
		return
	}

	if err := s.startSession(w, r, user); err != nil {
		s.Log.Error("start session", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.InvalidateSetupCache()
	// Rails redirects to the stored return URL or admin root; the Go rewrite
	// has no return-to store yet, so it always lands on the admin root.
	http.Redirect(w, r, "/admin/", http.StatusFound)
}

// logout handles POST /session/destroy, mirroring SessionsController#destroy
// (Rails routes it as DELETE /session; HTML forms can only POST).
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		if err := s.Q.DeleteSessionByToken(r.Context(), cookie.Value); err != nil {
			s.Log.Error("delete session", "error", err)
		}
	}
	clearSessionCookie(w, s.Cfg.SecureCookies)
	s.InvalidateSetupCache()
	http.Redirect(w, r, "/", http.StatusFound)
}

// passwordPageData feeds auth_password_edit.html.
type passwordPageData struct {
	Flash templates.Flash
	User  query.User
}

// userEditForm renders GET /users/{id}/edit. Like UsersController#edit it
// always edits the current user; the path id is ignored.
func (s *Server) userEditForm(w http.ResponseWriter, r *http.Request) {
	user, _ := CurrentUser(r)
	s.render(w, http.StatusOK, "auth_password_edit", passwordPageData{
		Flash: s.PopFlash(r, w),
		User:  user,
	})
}

// userUpdate handles POST /users/{id} (Rails PATCH /users/:id; HTML forms
// cannot PATCH). Mirrors UsersController#update: changing the password
// requires the current password; a blank new password leaves it unchanged.
func (s *Server) userUpdate(w http.ResponseWriter, r *http.Request) {
	user, _ := CurrentUser(r)
	if !parseCappedForm(w, r) {
		return
	}
	userName := normalizeUserName(r.FormValue("user_name"))
	currentPassword := r.FormValue("current_password")
	password := r.FormValue("password")
	confirmation := r.FormValue("password_confirmation")

	fail := func(alert string) {
		s.render(w, http.StatusUnprocessableEntity, "auth_password_edit", passwordPageData{
			Flash: templates.Flash{Alert: alert},
			User:  user,
		})
	}

	if userName == "" {
		fail("User name can't be blank.")
		return
	}
	digest := user.PasswordDigest
	if password != "" {
		if bcrypt.CompareHashAndPassword([]byte(user.PasswordDigest), []byte(currentPassword)) != nil {
			fail("Current password is incorrect.")
			return
		}
		if password != confirmation {
			fail("Password confirmation doesn't match password.")
			return
		}
		// bcrypt rejects inputs over 72 bytes; Rails' has_secure_password
		// validates the length instead, so mirror it as a form error.
		if len(password) > 72 {
			fail("Password is too long (maximum is 72 characters).")
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
		if err != nil {
			s.Log.Error("hash password", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		digest = string(hash)
	}

	err := s.Q.UpdateUser(r.Context(), query.UpdateUserParams{
		UserName:       userName,
		PasswordDigest: digest,
		UpdatedAt:      time.Now().Unix(),
		ID:             user.ID,
	})
	if err != nil {
		if isUniqueViolation(err) {
			fail("User name has already been taken.")
			return
		}
		s.Log.Error("update user", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// A password change revokes the user's other sessions: a stolen cookie
	// must stop working once the owner resets the password. The current
	// session survives so the owner stays logged in.
	if password != "" {
		if cookie, err := r.Cookie(sessionCookieName); err == nil {
			if err := s.Q.DeleteOtherSessionsForUser(r.Context(), query.DeleteOtherSessionsForUserParams{
				UserID: user.ID,
				Token:  cookie.Value,
			}); err != nil {
				s.Log.Error("delete other sessions", "error", err)
			}
		}
	}

	s.SetFlash(w, templates.Flash{Notice: "Account was successfully updated."})
	http.Redirect(w, r, "/admin/posts", http.StatusFound)
}

// startSession creates the sessions row and sets the cookie, mirroring
// start_new_session_for (token-based instead of Rails' signed id).
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, user query.User) error {
	token, err := newSessionToken()
	if err != nil {
		return err
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	now := time.Now().Unix()
	_, err = s.Q.CreateSession(r.Context(), query.CreateSessionParams{
		Token:     token,
		UserID:    user.ID,
		IpAddress: sql.NullString{String: ip, Valid: ip != ""},
		UserAgent: sql.NullString{String: r.UserAgent(), Valid: r.UserAgent() != ""},
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.Cfg.SecureCookies,
	})
	return nil
}

func clearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}

// newSessionToken returns 32 random bytes hex-encoded (64 chars).
func newSessionToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// normalizeUserName mirrors User.normalizes (:user_name strip + downcase).
func normalizeUserName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// isUniqueViolation reports a SQLite UNIQUE constraint failure.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// render executes a page template, logging failures.
func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	if s.Renderer == nil {
		http.Error(w, "renderer not configured", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.Renderer.Render(w, name, data); err != nil {
		s.Log.Error("render", "page", name, "error", err)
	}
}
