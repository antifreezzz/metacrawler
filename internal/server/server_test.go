package server_test

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"metacrawler/internal/config"
	"metacrawler/internal/domain"
	"metacrawler/internal/llm"
	"metacrawler/internal/server"
	"metacrawler/internal/storage"
	"metacrawler/internal/worker"
)

type dummyScraper struct{}

func (d *dummyScraper) FetchNewReleases(ctx context.Context) ([]string, error) {
	return nil, nil
}
func (d *dummyScraper) FetchBrowsePage(ctx context.Context, page int) ([]string, error) {
	return nil, nil
}
func (d *dummyScraper) FetchGameDetails(ctx context.Context, slug string) (*domain.Game, []domain.Review, error) {
	return nil, nil, nil
}

func setupServer(t *testing.T) (*server.Server, *storage.DB) {
	db, err := storage.New(":memory:")
	require.NoError(t, err)

	cfg := &config.Config{Port: "8079"}
	llmClient := llm.NewClient("http://mock/v1", "", "gpt-4o-mini", "text-embedding-3-small")
	mgr := worker.NewManager(db, &dummyScraper{}, llmClient, nil, cfg)

	srv := server.New(db, mgr, llmClient, cfg)
	return srv, db
}

func seedTestData(t *testing.T, db *storage.DB) {
	ctx := context.Background()
	score95 := 95
	user9 := 9.0

	g1 := &domain.Game{
		ID:          "g-1",
		Slug:        "elden-ring",
		Title:       "Elden Ring",
		Description: "A great dark fantasy RPG.",
		Developer:   "FromSoftware",
		Platforms: []domain.GamePlatform{
			{Platform: "ps5", Metascore: &score95, Userscore: &user9},
		},
	}
	require.NoError(t, db.UpsertGame(ctx, g1))

	// Добавим эмбеддинг
	require.NoError(t, db.SaveEmbedding(ctx, &domain.GameEmbedding{
		GameID:     g1.ID,
		Vector:     []float32{1.0, 0.0, 0.0},
		Dimensions: 3,
	}))

	g2 := &domain.Game{
		ID:          "g-2",
		Slug:        "dark-souls-3",
		Title:       "Dark Souls III",
		Description: "Another great soulslike.",
		Developer:   "FromSoftware",
		Platforms: []domain.GamePlatform{
			{Platform: "pc", Metascore: &score95},
		},
	}
	require.NoError(t, db.UpsertGame(ctx, g2))

	require.NoError(t, db.SaveEmbedding(ctx, &domain.GameEmbedding{
		GameID:     g2.ID,
		Vector:     []float32{0.9, 0.1, 0.0},
		Dimensions: 3,
	}))
}

func TestIndexHandler_Returns200(t *testing.T) {
	srv, db := setupServer(t)
	defer db.Close()
	seedTestData(t, db)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "Metacrawler")
	require.Contains(t, rec.Body.String(), "Elden Ring")
}

func TestIndexHandler_OpenGraphDefaults(t *testing.T) {
	srv, db := setupServer(t)
	defer db.Close()
	seedTestData(t, db)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	body := rec.Body.String()

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, body, `<meta property="og:title"`)
	require.Contains(t, body, `<meta property="og:site_name" content="Metacrawler" />`)
	require.Contains(t, body, `type="application/rss+xml"`)
	require.Contains(t, body, `rel="icon"`)
}

func TestGameDetailHandler_DescriptionParagraphs(t *testing.T) {
	// Описание из JSON-LD Metacritic содержит переносы абзацев (\n\n),
	// каждое \"перенос-разделённое\" предложение должно стать отдельным <p>.
	srv, db := setupServer(t)
	defer db.Close()

	ctx := context.Background()
	game := &domain.Game{
		ID:          "g-para",
		Slug:        "para-game",
		Title:       "Para Game",
		Description: "First paragraph.\n\nSecond paragraph.\n\nThird one.",
	}
	require.NoError(t, db.UpsertGame(ctx, game))

	req := httptest.NewRequest("GET", "/games/para-game", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	body := rec.Body.String()

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, body, "<p>First paragraph.</p>")
	require.Contains(t, body, "<p>Second paragraph.</p>")
	require.Contains(t, body, "<p>Third one.</p>")
	// Сырые переносы и слитный текст в одном <p> не должны остаться
	require.NotContains(t, body, "paragraph.\n\nSecond")
}

func TestGameDetailHandler_ShowsRussianDescriptionWithOriginal(t *testing.T) {
	srv, db := setupServer(t)
	defer db.Close()

	ctx := context.Background()
	game := &domain.Game{
		ID:          "g-ru",
		Slug:        "ru-desc-game",
		Title:       "RU Desc Game",
		Description: "Original English description.",
	}
	require.NoError(t, db.UpsertGame(ctx, game))
	saved, err := db.GetGameBySlug(ctx, "ru-desc-game")
	require.NoError(t, err)
	require.NoError(t, db.SaveGameTranslation(ctx, saved.ID, "Русское описание.\n\nВторой абзац."))

	req := httptest.NewRequest("GET", "/games/ru-desc-game", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	body := rec.Body.String()

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, body, "<p>Русское описание.</p>")
	require.Contains(t, body, "<p>Второй абзац.</p>")
	require.Contains(t, body, "original-description", "оригинал должен быть в сворачиваемом блоке")
	require.Contains(t, body, "<p>Original English description.</p>")
	require.Contains(t, body, `<meta property="og:description" content="Русское описание. Второй абзац." />`)
}

func TestGameDetailHandler_OpenGraphTags(t *testing.T) {
	srv, db := setupServer(t)
	defer db.Close()

	ctx := context.Background()
	game := &domain.Game{
		ID:          "g-og",
		Slug:        "og-game",
		Title:       "OG Game",
		Description: "Description with \"quotes\" & symbols",
		CoverURL:    "https://example.com/cover.jpg",
	}
	require.NoError(t, db.UpsertGame(ctx, game))

	req := httptest.NewRequest("GET", "/games/og-game", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	body := rec.Body.String()

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, body, `<meta property="og:title" content="OG Game — Metacrawler" />`)
	require.Contains(t, body, `<meta property="og:description" content="Description with &#34;quotes&#34; &amp; symbols" />`)
	require.Contains(t, body, `<meta property="og:image" content="https://example.com/cover.jpg" />`)
	require.Contains(t, body, `<meta property="og:url" content="http://example.com/games/og-game" />`)
	require.Contains(t, body, `<meta name="twitter:card" content="summary_large_image" />`)
}

func TestRSSFeed_ReturnsValidXML(t *testing.T) {
	srv, db := setupServer(t)
	defer db.Close()
	seedTestData(t, db)

	req := httptest.NewRequest("GET", "/rss", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	body := rec.Body.String()

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "application/rss+xml")

	var feed rssFeed
	err := xml.Unmarshal([]byte(body), &feed)
	require.NoError(t, err, "feed must be valid XML: %s", body)
	require.Equal(t, "Metacrawler — новые игры", feed.Channel.Title)
	require.Len(t, feed.Channel.Items, 2)

	slugs := []string{feed.Channel.Items[0].Link, feed.Channel.Items[1].Link}
	require.Contains(t, slugs, "/games/elden-ring")
	require.Contains(t, slugs, "/games/dark-souls-3")
	require.NotEmpty(t, feed.Channel.Items[0].PubDate)
}

type rssFeed struct {
	XMLName xml.Name `xml:"rss"`
	Version string   `xml:"version,attr"`
	Channel struct {
		Title string    `xml:"title"`
		Link  string    `xml:"link"`
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	PubDate     string `xml:"pubDate"`
	Description string `xml:"description"`
}

func TestRSSFeed_EscapesSpecialChars(t *testing.T) {
	srv, db := setupServer(t)
	defer db.Close()

	ctx := context.Background()
	game := &domain.Game{
		ID:          "g-rss",
		Slug:        "rss-game",
		Title:       "Game <X> & \"Weird\"",
		Description: "Desc with <tags> & ampersands",
	}
	require.NoError(t, db.UpsertGame(ctx, game))

	req := httptest.NewRequest("GET", "/rss", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	var feed rssFeed
	err := xml.Unmarshal(rec.Body.Bytes(), &feed)
	require.NoError(t, err, "special chars must be escaped, body: %s", rec.Body.String())
	require.Contains(t, feed.Channel.Items[0].Title, "Game <X> & \"Weird\"")
}

func TestGameDetailHandler_NoSummary_ShowsRealQuotesNotFake(t *testing.T) {
	srv, db := setupServer(t)
	defer db.Close()

	ctx := context.Background()
	game := &domain.Game{
		ID:          "g-quotes",
		Slug:        "quotes-game",
		Title:       "Quotes Game",
		Description: "Game with reviews but no LLM summary.",
		Platforms: []domain.GamePlatform{
			{Platform: "pc"},
		},
	}
	require.NoError(t, db.UpsertGame(ctx, game))
	saved, err := db.GetGameBySlug(ctx, "quotes-game")
	require.NoError(t, err)

	reviews := []domain.Review{
		{GamePlatformID: saved.Platforms[0].ID, ReviewType: domain.ReviewTypeCritic, Author: "RealCritic", Text: "The level design is genuinely brilliant and inventive."},
	}
	_, err = db.SaveReviews(ctx, reviews)
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "/games/quotes-game", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	body := rec.Body.String()

	require.Equal(t, http.StatusOK, rec.Code)
	// Реальные цитаты отзывов показываем
	require.Contains(t, body, "RealCritic")
	require.Contains(t, body, "The level design is genuinely brilliant and inventive.")
	// Никаких обещаний, что анализ «формируется»
	require.NotContains(t, body, "формируется")
}

func TestGameDetailHandler_ShowsScoreDelta(t *testing.T) {
	srv, db := setupServer(t)
	defer db.Close()

	ctx := context.Background()
	score90 := 90
	game := &domain.Game{
		ID:    "g-delta",
		Slug:  "delta-game",
		Title: "Delta Game",
		Platforms: []domain.GamePlatform{
			{Platform: "pc", Metascore: &score90},
		},
	}
	require.NoError(t, db.UpsertGame(ctx, game))
	saved, err := db.GetGameBySlug(ctx, "delta-game")
	require.NoError(t, err)

	// История: 88 -> (upsert) -> 90
	s1, s2 := 88, 90
	_ = s2
	require.NoError(t, db.RecordScorePoint(ctx, saved.Platforms[0].ID, &s1, nil))
	require.NoError(t, db.RecordScorePoint(ctx, saved.Platforms[0].ID, &score90, nil))

	req := httptest.NewRequest("GET", "/games/delta-game", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	body := rec.Body.String()

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, body, "score-delta up")
	// html/template экранирует "+" как &#43; в текстовом узле
	require.Contains(t, body, "&#43;2")
}

func TestGameListPartial_FiltersResults(t *testing.T) {
	srv, db := setupServer(t)
	defer db.Close()
	seedTestData(t, db)

	req := httptest.NewRequest("GET", "/api/games?search=Dark", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "Dark Souls III")
	require.NotContains(t, rec.Body.String(), "Elden Ring")
	require.Equal(t, "/?search=Dark", rec.Header().Get("HX-Replace-Url"))
}

func TestGameDetailHandler_ReturnsDetailsAndSimilar(t *testing.T) {
	srv, db := setupServer(t)
	defer db.Close()
	seedTestData(t, db)

	req := httptest.NewRequest("GET", "/games/elden-ring", nil)
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "Elden Ring")
	require.Contains(t, rec.Body.String(), "FromSoftware")
	// Должна отображаться похожая игра
	require.Contains(t, rec.Body.String(), "Dark Souls III")
}

func TestMonitoringHandler_Returns200(t *testing.T) {
	srv, db := setupServer(t)
	defer db.Close()

	req := httptest.NewRequest("GET", "/monitoring", nil)
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "Мониторинг сервиса")
	require.Contains(t, rec.Body.String(), "Запустить сбор")
}

func TestWorkerRunEndpoint_ForcedModes(t *testing.T) {
	srv, db := setupServer(t)
	defer db.Close()

	// Запуск New Releases
	req := httptest.NewRequest("POST", "/api/worker/run?mode=new_releases", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Contains(t, rec.Body.String(), `"status":"accepted"`)
	require.Contains(t, rec.Body.String(), `"mode":"new_releases"`)

	// Запрос статуса
	reqStatus := httptest.NewRequest("GET", "/api/worker/status", nil)
	recStatus := httptest.NewRecorder()
	srv.Router().ServeHTTP(recStatus, reqStatus)

	require.Equal(t, http.StatusOK, recStatus.Code)
	require.Contains(t, recStatus.Body.String(), `"status"`)
}

func TestIndexHandler_Pagination(t *testing.T) {
	srv, db := setupServer(t)
	defer db.Close()
	ctx := context.Background()

	for i := 1; i <= 30; i++ {
		_ = db.UpsertGame(ctx, &domain.Game{
			ID:          fmt.Sprintf("g-%d", i),
			Slug:        fmt.Sprintf("game-%d", i),
			Title:       fmt.Sprintf("Game %02d", i),
			ReleaseDate: fmt.Sprintf("2024-01-%02d", i),
		})
	}

	// Page 1
	req1 := httptest.NewRequest("GET", "/?page=1", nil)
	rec1 := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Code)
	require.Contains(t, rec1.Body.String(), "1–24")
	require.Contains(t, rec1.Body.String(), "из 30")
	require.Contains(t, rec1.Body.String(), "Game 30")

	// Page 2
	req2 := httptest.NewRequest("GET", "/?page=2", nil)
	rec2 := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusOK, rec2.Code)
	require.Contains(t, rec2.Body.String(), "25–30")
	require.Contains(t, rec2.Body.String(), "Game 01")
}

func TestGameDetailHandler_OnDemandSummary(t *testing.T) {
	srv, db := setupServer(t)
	defer db.Close()
	ctx := context.Background()

	g := &domain.Game{
		ID:    "g-ondemand",
		Slug:  "ondemand-game",
		Title: "On Demand Game",
		Platforms: []domain.GamePlatform{
			{Platform: "pc"},
		},
	}
	require.NoError(t, db.UpsertGame(ctx, g))
	saved, _ := db.GetGameBySlug(ctx, "ondemand-game")
	platID := saved.Platforms[0].ID

	// Сохраняем отзывы, но НЕ создаем summary
	revs := []domain.Review{
		{GamePlatformID: platID, ReviewType: domain.ReviewTypeCritic, Author: "IGN", Text: "Superb action!", ContentHash: "h1"},
		{GamePlatformID: platID, ReviewType: domain.ReviewTypeUser, Author: "Player", Text: "Awesome story!", ContentHash: "h2"},
	}
	_, err := db.SaveReviews(ctx, revs)
	require.NoError(t, err)

	// Проверяем, что summary изначально отсутствует
	sumBefore, _ := db.GetPlatformSummary(ctx, platID)
	require.Nil(t, sumBefore)

	// Открываем детальную страницу
	req := httptest.NewRequest("GET", "/games/ondemand-game", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// Честное поведение: без подключенной LLM резюме не выдумывается,
	// страница показывает реальные цитаты отзывов
	sumAfter, err := db.GetPlatformSummary(ctx, platID)
	require.NoError(t, err)
	require.Nil(t, sumAfter, "без LLM резюме не генерируется")

	body := rec.Body.String()
	require.Contains(t, body, "IGN")
	require.Contains(t, body, "Superb action!")
	require.Contains(t, body, "недоступен")
}

func TestGameRecrawlEndpoint_RequiresAuthAndExecutes(t *testing.T) {
	db, err := storage.New(":memory:")
	require.NoError(t, err)
	defer db.Close()

	cfg := &config.Config{
		Port:          "8079",
		AdminUsername: "admin",
		AdminPassword: "secret-password",
		SessionSecret: "secret-key",
	}
	llmClient := llm.NewClient("http://mock/v1", "", "gpt-4o-mini", "text-embedding-3-small")
	mgr := worker.NewManager(db, &dummyScraper{}, llmClient, nil, cfg)
	srv := server.New(db, mgr, llmClient, cfg)

	// 1. Без авторизации - 401 Unauthorized
	req := httptest.NewRequest("POST", "/api/games/recrawl-game/recrawl", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// 2. С авторизацией Basic Auth - возвращает 202 Accepted
	reqAuth := httptest.NewRequest("POST", "/api/games/recrawl-game/recrawl", nil)
	reqAuth.SetBasicAuth("admin", "secret-password")
	reqAuth.Header.Set("HX-Request", "true")
	recAuth := httptest.NewRecorder()
	srv.Router().ServeHTTP(recAuth, reqAuth)
	require.Equal(t, http.StatusAccepted, recAuth.Code)
}

func TestRecrawlStatusEndpoint(t *testing.T) {
	db, err := storage.New(":memory:")
	require.NoError(t, err)
	defer db.Close()

	cfg := &config.Config{
		Port:          "8079",
		AdminUsername: "admin",
		AdminPassword: "secret-password",
		SessionSecret: "secret-key",
	}
	llmClient := llm.NewClient("http://mock/v1", "", "gpt-4o-mini", "text-embedding-3-small")
	mgr := worker.NewManager(db, &dummyScraper{}, llmClient, nil, cfg)
	srv := server.New(db, mgr, llmClient, cfg)

	// 1. Неизвестный slug -> idle
	reqIdle := httptest.NewRequest("GET", "/api/games/unknown-game/recrawl/status", nil)
	reqIdle.SetBasicAuth("admin", "secret-password")
	recIdle := httptest.NewRecorder()
	srv.Router().ServeHTTP(recIdle, reqIdle)
	require.Equal(t, http.StatusOK, recIdle.Code)
	require.Contains(t, recIdle.Body.String(), `"status":"idle"`)

	// 2. Статус требует авторизации
	reqNoAuth := httptest.NewRequest("GET", "/api/games/unknown-game/recrawl/status", nil)
	recNoAuth := httptest.NewRecorder()
	srv.Router().ServeHTTP(recNoAuth, reqNoAuth)
	require.Equal(t, http.StatusUnauthorized, recNoAuth.Code)

	// 3. Запуск пересбора выбранных частей -> 202 и options в ответе
	reqStart := httptest.NewRequest("POST", "/api/games/dummy-game/recrawl?parts=summaries,embedding", nil)
	reqStart.SetBasicAuth("admin", "secret-password")
	recStart := httptest.NewRecorder()
	srv.Router().ServeHTTP(recStart, reqStart)
	require.Equal(t, http.StatusAccepted, recStart.Code)
	require.Contains(t, recStart.Body.String(), "summaries,embedding")

	// 4. Статус запущенной игры содержит slug и выбранные части
	reqStatus := httptest.NewRequest("GET", "/api/games/dummy-game/recrawl/status", nil)
	reqStatus.SetBasicAuth("admin", "secret-password")
	recStatus := httptest.NewRecorder()
	srv.Router().ServeHTTP(recStatus, reqStatus)
	require.Equal(t, http.StatusOK, recStatus.Code)
	require.Contains(t, recStatus.Body.String(), `"slug":"dummy-game"`)
	require.Contains(t, recStatus.Body.String(), "summaries,embedding")

	// 5. Пустой набор частей (неизвестные значения) -> 400
	reqBad := httptest.NewRequest("POST", "/api/games/dummy-game/recrawl?parts=unknown", nil)
	reqBad.SetBasicAuth("admin", "secret-password")
	recBad := httptest.NewRecorder()
	srv.Router().ServeHTTP(recBad, reqBad)
	require.Equal(t, http.StatusBadRequest, recBad.Code)
}

func TestRecrawlTranslationPart_Accepted(t *testing.T) {
	db, err := storage.New(":memory:")
	require.NoError(t, err)
	defer db.Close()

	cfg := &config.Config{
		Port:          "8079",
		AdminUsername: "admin",
		AdminPassword: "secret-password",
		SessionSecret: "secret-key",
	}
	llmClient := llm.NewClient("http://mock/v1", "", "gpt-4o-mini", "text-embedding-3-small")
	mgr := worker.NewManager(db, &dummyScraper{}, llmClient, nil, cfg)
	srv := server.New(db, mgr, llmClient, cfg)

	req := httptest.NewRequest("POST", "/api/games/dummy-game/recrawl?parts=translation", nil)
	req.SetBasicAuth("admin", "secret-password")
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Contains(t, rec.Body.String(), "translation")
}

func TestLogin_OpenRedirectPrevention(t *testing.T) {
	db, err := storage.New(":memory:")
	require.NoError(t, err)
	defer db.Close()

	cfg := &config.Config{
		Port:          "8079",
		AdminUsername: "admin",
		AdminPassword: "password123",
		SessionSecret: "secret-key",
	}
	llmClient := llm.NewClient("http://mock/v1", "", "gpt-4o-mini", "text-embedding-3-small")
	mgr := worker.NewManager(db, &dummyScraper{}, llmClient, nil, cfg)
	srv := server.New(db, mgr, llmClient, cfg)

	maliciousTargets := []string{
		"https://evil.com/phishing",
		"http://attacker.com",
		"//evil.com",
		"/\\evil.com",
		"javascript:alert(1)",
	}

	for _, target := range maliciousTargets {
		// 1. GET /login?next=... должен отсанитизировать значение в HTML
		reqGet := httptest.NewRequest("GET", "/login?next="+target, nil)
		recGet := httptest.NewRecorder()
		srv.Router().ServeHTTP(recGet, reqGet)
		require.Equal(t, http.StatusOK, recGet.Code)
		require.NotContains(t, recGet.Body.String(), `value="`+target+`"`)
		require.Contains(t, recGet.Body.String(), `value="/monitoring"`)

		// 2. POST /login с вредоносным next должен редиректить на /monitoring, а не во внешний мир
		formBody := "username=admin&password=password123&next=" + target
		reqPost := httptest.NewRequest("POST", "/login", strings.NewReader(formBody))
		reqPost.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recPost := httptest.NewRecorder()
		srv.Router().ServeHTTP(recPost, reqPost)
		require.Equal(t, http.StatusFound, recPost.Code)
		require.Equal(t, "/monitoring", recPost.Header().Get("Location"))
	}

	// 3. Валидный относительный URL должен работать корректно
	formValid := "username=admin&password=password123&next=/games/elden-ring"
	reqValid := httptest.NewRequest("POST", "/login", strings.NewReader(formValid))
	reqValid.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recValid := httptest.NewRecorder()
	srv.Router().ServeHTTP(recValid, reqValid)
	require.Equal(t, http.StatusFound, recValid.Code)
	require.Equal(t, "/games/elden-ring", recValid.Header().Get("Location"))
}
