package proxy

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"model-router/internal/config"
)

// §24.7 D4: cookie sessions replace HTTP Basic for browsers. The store lives in
// process memory only (restart invalidates every session), and tokens are opaque
// CSPRNG values — never credentials or their hashes.
const (
	PathLogin     = "/api/login"
	PathLogout    = "/api/logout"
	PathLoginPage = "/admin/login"

	// PathLogo is the brand mark served anonymously (§24.14 L1).
	PathLogo = "/logo.svg"

	sessionCookieName  = "mr_session"
	sessionTokenBytes  = 32
	sessionRememberTTL = 30 * 24 * time.Hour
	sessionDefaultTTL  = 12 * time.Hour
)

type sessionEntry struct {
	user      string
	expiresAt time.Time
}

type sessionStore struct {
	mu      sync.RWMutex
	entries map[string]sessionEntry
	now     func() time.Time
}

func newSessionStore() *sessionStore {
	return &sessionStore{entries: make(map[string]sessionEntry), now: time.Now}
}

// create stores a session for the given user. The server-side lifetime is 30
// days when "remember" was checked and 12 hours otherwise (§24.7 D4).
func (s *sessionStore) create(token, user string, remember bool) {
	ttl := sessionDefaultTTL
	if remember {
		ttl = sessionRememberTTL
	}
	s.mu.Lock()
	s.entries[token] = sessionEntry{user: user, expiresAt: s.now().Add(ttl)}
	s.mu.Unlock()
}

// authorized reports whether the request carries a live mr_session cookie and
// purges expired entries while it is there (§24.7 D4 expiry cleanup).
func (s *sessionStore) authorized(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, entry := range s.entries {
		if !now.Before(entry.expiresAt) {
			delete(s.entries, token)
		}
	}
	entry, ok := s.entries[cookie.Value]
	return ok && now.Before(entry.expiresAt)
}

func (s *sessionStore) remove(token string) {
	s.mu.Lock()
	delete(s.entries, token)
	s.mu.Unlock()
}

func newSessionToken() (string, error) {
	raw := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// safeNext only accepts in-site admin paths, rejecting protocol-relative and
// absolute URLs so ?next= cannot become an open redirect (§24.7 D6).
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/admin") {
		return "/admin/"
	}
	if strings.HasPrefix(next, "//") || strings.Contains(next, "://") || strings.Contains(next, `\`) {
		return "/admin/"
	}
	parsed, err := url.Parse(next)
	if err != nil || parsed.IsAbs() || parsed.Host != "" {
		return "/admin/"
	}
	return next
}

type loginPayload struct {
	User     string `json:"user"`
	Password string `json:"password"`
	Remember bool   `json:"remember"`
}

type loginResponse struct {
	Redirect string `json:"redirect"`
}

func (s *Server) serveLogin(w http.ResponseWriter, r *http.Request, cfg *config.Config) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAdminError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var payload loginPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&payload); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid login request")
		return
	}
	if !adminCredentialsMatch(cfg, payload.User, payload.Password) {
		writeAdminError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	token, err := newSessionToken()
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "cannot create session")
		return
	}
	s.sessions.create(token, payload.User, payload.Remember)
	cookie := &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// No Secure: local HTTP deployments must be able to log in (§24.7 D4).
	}
	if payload.Remember {
		cookie.MaxAge = int(sessionRememberTTL / time.Second)
	}
	http.SetCookie(w, cookie)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(loginResponse{Redirect: safeNext(r.URL.Query().Get("next"))})
}

func (s *Server) serveLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAdminError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		s.sessions.remove(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	writeAdminOK(w)
}

func isLoginPath(path string) bool {
	return path == PathLogin || path == PathLoginPage
}

func isWSPath(path string) bool {
	return path == PathWS
}
