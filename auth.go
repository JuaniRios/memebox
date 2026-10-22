package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const sessionCookie = "memebox_session"

// Auth is a single shared password. With no password set the app is open,
// which is the intended setup behind a tailnet or another private network.
type Auth struct {
	password string
	session  string
}

func NewAuth(password, dataDir string) (*Auth, error) {
	a := &Auth{password: password}
	if password == "" {
		return a, nil
	}
	secretPath := filepath.Join(dataDir, "session-secret")
	secret, err := os.ReadFile(secretPath)
	if errors.Is(err, os.ErrNotExist) {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
		if err := os.WriteFile(secretPath, secret, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(password))
	a.session = hex.EncodeToString(mac.Sum(nil))
	return a, nil
}

func (a *Auth) Enabled() bool { return a.password != "" }

func (a *Auth) Check(r *http.Request) bool {
	if !a.Enabled() {
		return true
	}
	if c, err := r.Cookie(sessionCookie); err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(a.session)) == 1 {
		return true
	}
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return subtle.ConstantTimeCompare([]byte(bearer), []byte(a.password)) == 1
	}
	return false
}

func (a *Auth) Login(w http.ResponseWriter, r *http.Request, password string) bool {
	if subtle.ConstantTimeCompare([]byte(password), []byte(a.password)) != 1 {
		time.Sleep(time.Second)
		return false
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: a.session, Path: "/", HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode, MaxAge: 400 * 24 * 3600,
	})
	return true
}

// Public paths load without a session so the login page renders and the
// browser can still read the manifest and icons to install the app.
func isPublicPath(p string) bool {
	switch p {
	case "/login", "/style.css", "/manifest.webmanifest", "/sw.js", "/healthz":
		return true
	}
	return strings.HasPrefix(p, "/icons/")
}

func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Check(r) || isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/media/") {
			writeErr(w, http.StatusUnauthorized, "log in first")
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}
