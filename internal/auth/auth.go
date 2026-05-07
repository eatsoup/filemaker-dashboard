package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"

	"filemaker-dashboard/internal/store"
)

const CookieName = "fmsession"

type Manager struct {
	Store      *store.Store
	SessionTTL time.Duration
}

// HashPassword returns a bcrypt hash for the given plaintext.
func HashPassword(pw string) (string, error) {
	if pw == "" {
		return "", errors.New("password must not be empty")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Login verifies credentials and creates a web session, returning the token.
func (m *Manager) Login(username, password string) (string, *store.User, error) {
	u, err := m.Store.UserByUsername(username)
	if err != nil {
		return "", nil, err
	}
	if u == nil || !CheckPassword(u.PasswordHash, password) {
		return "", nil, errors.New("invalid credentials")
	}
	tok, err := newToken()
	if err != nil {
		return "", nil, err
	}
	if err := m.Store.CreateWebSession(tok, u.ID, time.Now().Add(m.SessionTTL)); err != nil {
		return "", nil, err
	}
	return tok, u, nil
}

func (m *Manager) Logout(token string) error {
	return m.Store.DeleteWebSession(token)
}

// SetSessionCookie sets the auth cookie. SameSite=Lax + HttpOnly.
// Secure is set automatically if request was https.
func SetSessionCookie(w http.ResponseWriter, r *http.Request, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(ttl),
		MaxAge:   int(ttl.Seconds()),
	})
}

func ClearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// EnsureAdminFromConfig creates the configured admin user on first run if no
// users exist yet. Subsequent runs are a no-op so passwords can be changed in
// the UI without being clobbered by the config file.
func (m *Manager) EnsureAdminFromConfig(username, password string) error {
	n, err := m.Store.CountUsers()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	if _, err := m.Store.CreateUser(username, hash, true); err != nil {
		return fmt.Errorf("create admin user: %w", err)
	}
	return nil
}

// userCtxKey is the request-context key for the authenticated user.
type userCtxKey struct{}

// FromContext returns the user attached to the request, or nil if unauthenticated.
func FromContext(r *http.Request) *store.User {
	u, _ := r.Context().Value(userCtxKey{}).(*store.User)
	return u
}
