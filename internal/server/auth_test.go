package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"metacrawler/internal/config"
	"metacrawler/internal/domain"
	"metacrawler/internal/llm"
	"metacrawler/internal/server"
	"metacrawler/internal/storage"
	"metacrawler/internal/worker"
)

type mockScraper struct{}

func (m *mockScraper) FetchNewReleases(ctx context.Context) ([]string, error) {
	return []string{"game-1"}, nil
}
func (m *mockScraper) FetchBrowsePage(ctx context.Context, page int) ([]string, error) {
	return []string{"game-1"}, nil
}
func (m *mockScraper) FetchGameDetails(ctx context.Context, slug string) (*domain.Game, []domain.Review, error) {
	return &domain.Game{Slug: slug, Title: "Test"}, nil, nil
}

func setupAuthServer(t *testing.T, password string) (*server.Server, *storage.DB) {
	t.Helper()
	db, err := storage.New(":memory:")
	require.NoError(t, err)

	cfg := &config.Config{
		AdminUsername: "admin",
		AdminPassword: password,
		SessionSecret: "test-secret-key-98765",
	}

	llmClient := llm.NewClient("http://mock/v1", "", "gpt-4o-mini", "local")
	workerMgr := worker.NewManager(db, &mockScraper{}, llmClient, nil, cfg)
	srv := server.New(db, workerMgr, llmClient, cfg)
	return srv, db
}

func TestAuth_ProtectedEndpointsRequireAuth(t *testing.T) {
	srv, db := setupAuthServer(t, "supersecret")
	defer db.Close()

	// 1. Неавторизованный запрос к веб-интерфейсу мониторинга должен редиректить на /login
	req := httptest.NewRequest("GET", "/monitoring", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Contains(t, rec.Header().Get("Location"), "/login?next=/monitoring")

	// 2. Неавторизованный запрос к API воркера должен возвращать 401 Unauthorized
	reqAPI := httptest.NewRequest("POST", "/api/worker/run", nil)
	recAPI := httptest.NewRecorder()
	srv.Router().ServeHTTP(recAPI, reqAPI)
	require.Equal(t, http.StatusUnauthorized, recAPI.Code)

	// 3. Публичные маршруты доступны гостям без пароля
	reqIndex := httptest.NewRequest("GET", "/", nil)
	recIndex := httptest.NewRecorder()
	srv.Router().ServeHTTP(recIndex, reqIndex)
	require.Equal(t, http.StatusOK, recIndex.Code)
}

func TestAuth_LoginFlowAndSessionCookie(t *testing.T) {
	srv, db := setupAuthServer(t, "supersecret")
	defer db.Close()

	// 1. Попытка входа с неверным паролем
	form := url.Values{
		"username": {"admin"},
		"password": {"wrongpass"},
		"next":     {"/monitoring"},
	}
	reqBad := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	reqBad.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recBad := httptest.NewRecorder()
	srv.Router().ServeHTTP(recBad, reqBad)
	require.Equal(t, http.StatusUnauthorized, recBad.Code)
	require.Contains(t, recBad.Body.String(), "Неверное имя пользователя или пароль")

	// 2. Успешный вход с правильным паролем
	formGood := url.Values{
		"username": {"admin"},
		"password": {"supersecret"},
		"next":     {"/monitoring"},
	}
	reqGood := httptest.NewRequest("POST", "/login", strings.NewReader(formGood.Encode()))
	reqGood.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recGood := httptest.NewRecorder()
	srv.Router().ServeHTTP(recGood, reqGood)

	require.Equal(t, http.StatusFound, recGood.Code)
	require.Equal(t, "/monitoring", recGood.Header().Get("Location"))

	// Проверка установки cookie
	cookies := recGood.Result().Cookies()
	require.NotEmpty(t, cookies)
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "metacrawler_session" {
			sessionCookie = c
			break
		}
	}
	require.NotNil(t, sessionCookie)
	require.True(t, sessionCookie.HttpOnly)

	// 3. Доступ к /monitoring с валидной cookie
	reqAuthed := httptest.NewRequest("GET", "/monitoring", nil)
	reqAuthed.AddCookie(sessionCookie)
	recAuthed := httptest.NewRecorder()
	srv.Router().ServeHTTP(recAuthed, reqAuthed)
	require.Equal(t, http.StatusOK, recAuthed.Code)
	require.Contains(t, recAuthed.Body.String(), "Мониторинг сервиса")

	// 4. Доступ к API через Basic Auth
	reqBasic := httptest.NewRequest("POST", "/api/worker/run?mode=new_releases", nil)
	reqBasic.SetBasicAuth("admin", "supersecret")
	recBasic := httptest.NewRecorder()
	srv.Router().ServeHTTP(recBasic, reqBasic)
	require.Equal(t, http.StatusAccepted, recBasic.Code)

	// 5. Логаут сбрасывает сессию
	reqLogout := httptest.NewRequest("GET", "/logout", nil)
	recLogout := httptest.NewRecorder()
	srv.Router().ServeHTTP(recLogout, reqLogout)
	require.Equal(t, http.StatusFound, recLogout.Code)

	logoutCookies := recLogout.Result().Cookies()
	require.NotEmpty(t, logoutCookies)
	require.True(t, logoutCookies[0].MaxAge < 0 || logoutCookies[0].Expires.Before(logoutCookies[0].Expires.Add(-time.Hour)))
}

func TestSecurityMiddleware_RejectsCrossOriginPost(t *testing.T) {
	srv, db := setupAuthServer(t, "supersecret")
	defer db.Close()

	form := url.Values{"username": {"admin"}, "password": {"supersecret"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestSecurityMiddleware_AllowsSameOriginPost(t *testing.T) {
	srv, db := setupAuthServer(t, "supersecret")
	defer db.Close()

	form := url.Values{"username": {"admin"}, "password": {"supersecret"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+req.Host)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusFound, rec.Code)
}

func TestLoginThrottle_BlocksAfterFailures(t *testing.T) {
	srv, db := setupAuthServer(t, "supersecret")
	defer db.Close()

	attempt := func() int {
		form := url.Values{"username": {"admin"}, "password": {"wrong"}}
		req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "10.0.0.5:1234"
		rec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rec, req)
		return rec.Code
	}

	for i := 0; i < 5; i++ {
		require.Equal(t, http.StatusUnauthorized, attempt(), "attempt %d", i+1)
	}
	require.Equal(t, http.StatusTooManyRequests, attempt())
}

func TestSessionCookie_HasSecureFlag(t *testing.T) {
	db, err := storage.New(":memory:")
	require.NoError(t, err)
	defer db.Close()

	cfg := &config.Config{
		AdminUsername: "admin",
		AdminPassword: "supersecret",
		SessionSecret: "test-secret-key-98765",
		CookieSecure:  true,
	}
	llmClient := llm.NewClient("http://mock/v1", "", "gpt-4o-mini", "local")
	workerMgr := worker.NewManager(db, &mockScraper{}, llmClient, nil, cfg)
	srv := server.New(db, workerMgr, llmClient, cfg)

	form := url.Values{"username": {"admin"}, "password": {"supersecret"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusFound, rec.Code)
	var sessionCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "metacrawler_session" {
			sessionCookie = c
		}
	}
	require.NotNil(t, sessionCookie)
	require.True(t, sessionCookie.Secure, "session cookie must be Secure")
}
