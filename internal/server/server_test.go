package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
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

	cfg := &config.Config{Port: "8080"}
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

func TestGameListPartial_FiltersResults(t *testing.T) {
	srv, db := setupServer(t)
	defer db.Close()
	seedTestData(t, db)

	req := httptest.NewRequest("GET", "/api/games?search=Dark", nil)
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "Dark Souls III")
	require.NotContains(t, rec.Body.String(), "Elden Ring")
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
	require.Contains(t, rec.Body.String(), "Принудительный запуск")
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
