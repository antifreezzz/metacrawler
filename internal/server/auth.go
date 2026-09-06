package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const sessionCookieName = "metacrawler_session"
const sessionDuration = 7 * 24 * time.Hour

type AuthManager struct {
	username string
	password string
	secret   []byte
}

func NewAuthManager(username, password, secret string) *AuthManager {
	if secret == "" {
		secret = "metacrawler-default-secret-key-12345"
	}
	return &AuthManager{
		username: username,
		password: password,
		secret:   []byte(secret),
	}
}

// IsAuthEnabled возвращает true, если задан пароль администратора.
func (a *AuthManager) IsAuthEnabled() bool {
	return a != nil && a.password != ""
}

// Authenticate проверяет переданные учетные данные.
func (a *AuthManager) Authenticate(username, password string) bool {
	if !a.IsAuthEnabled() {
		return true
	}
	userMatch := hmac.Equal([]byte(a.username), []byte(username))
	passMatch := hmac.Equal([]byte(a.password), []byte(password))
	return userMatch && passMatch
}

// GenerateSessionCookie генерирует защищенную подписанную сессионную cookie.
func (a *AuthManager) GenerateSessionCookie(username string) *http.Cookie {
	now := time.Now()
	exp := now.Add(sessionDuration).Unix()
	payload := fmt.Sprintf("%s:%d", username, exp)

	sig := a.sign(payload)
	val := fmt.Sprintf("%s:%s", payload, sig)

	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    val,
		Path:     "/",
		Expires:  now.Add(sessionDuration),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

// ClearSessionCookie возвращает cookie с истекшим сроком действия для логаута.
func (a *AuthManager) ClearSessionCookie() *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

// ValidateRequest проверяет cookie либо HTTP Basic Auth.
func (a *AuthManager) ValidateRequest(r *http.Request) bool {
	if !a.IsAuthEnabled() {
		return true
	}

	// 1. Проверка Basic Auth
	if user, pass, ok := r.BasicAuth(); ok {
		if a.Authenticate(user, pass) {
			return true
		}
	}

	// 2. Проверка cookie сессии
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}

	parts := strings.Split(cookie.Value, ":")
	if len(parts) != 3 {
		return false
	}

	username := parts[0]
	expStr := parts[1]
	sig := parts[2]

	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}

	expectedSig := a.sign(fmt.Sprintf("%s:%s", username, expStr))
	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return false
	}

	return username == a.username
}

func (a *AuthManager) sign(data string) string {
	h := hmac.New(sha256.New, a.secret)
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

// RequireAuth middleware защищает маршрут от неавторизованного доступа.
func (s *Server) RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.ValidateRequest(r) {
			// Если запрос API или HTMX — возвращаем 401
			if strings.HasPrefix(r.URL.Path, "/api/") || r.Header.Get("HX-Request") == "true" {
				w.Header().Set("WWW-Authenticate", `Basic realm="Metacrawler Admin"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			// Для браузерных страниц редиректим на логин
			nextURL := r.URL.RequestURI()
			http.Redirect(w, r, "/login?next="+nextURL, http.StatusFound)
			return
		}
		next(w, r)
	}
}
