package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"metacrawler/internal/config"
	"metacrawler/internal/domain"
	"metacrawler/internal/llm"
	"metacrawler/internal/storage"
	"metacrawler/internal/worker"
)

type MockScraper struct {
	mock.Mock
}

func (m *MockScraper) FetchNewReleases(ctx context.Context) ([]string, error) {
	args := m.Called(ctx)
	return args.Get(0).([]string), args.Error(1)
}

func (m *MockScraper) FetchBrowsePage(ctx context.Context, page int) ([]string, error) {
	args := m.Called(ctx, page)
	return args.Get(0).([]string), args.Error(1)
}

func (m *MockScraper) FetchGameDetails(ctx context.Context, slug string) (*domain.Game, []domain.Review, error) {
	args := m.Called(ctx, slug)
	return args.Get(0).(*domain.Game), args.Get(1).([]domain.Review), args.Error(2)
}

type MockLLM struct {
	mock.Mock
}

func (m *MockLLM) SummarizeReviews(ctx context.Context, title, platform string, critics, users []domain.Review) (*llm.SummaryResult, error) {
	args := m.Called(ctx, title, platform, critics, users)
	return args.Get(0).(*llm.SummaryResult), args.Error(1)
}

func (m *MockLLM) GetEmbedding(ctx context.Context, text string) ([]float32, error) {
	args := m.Called(ctx, text)
	return args.Get(0).([]float32), args.Error(1)
}

func setupWorkerEnv(t *testing.T) (*storage.DB, *MockScraper, *MockLLM, *worker.Manager) {
	db, err := storage.New(":memory:")
	require.NoError(t, err)

	cfg := &config.Config{
		CrawlDelayMinMs: 1, // быстрые тесты
		CrawlDelayMaxMs: 2,
	}

	scraperMock := new(MockScraper)
	llmMock := new(MockLLM)

	mgr := worker.NewManager(db, scraperMock, llmMock, nil, cfg)
	return db, scraperMock, llmMock, mgr
}

func sampleGame(slug string) (*domain.Game, []domain.Review) {
	score90 := 90
	game := &domain.Game{
		Slug:        slug,
		Title:       "Title " + slug,
		Description: "Description of " + slug,
		Platforms: []domain.GamePlatform{
			{Platform: "pc", Metascore: &score90, PlatformURL: "/game/" + slug + "/critic-reviews/?platform=pc"},
		},
	}
	reviews := []domain.Review{
		{ReviewType: domain.ReviewTypeCritic, Author: "Critic1", Text: "Good game"},
		{ReviewType: domain.ReviewTypeUser, Author: "User1", Text: "Awesome"},
	}
	return game, reviews
}

func TestWorker_FirstRunOfDay_NewReleases(t *testing.T) {
	db, scraperMock, llmMock, mgr := setupWorkerEnv(t)
	defer db.Close()

	ctx := context.Background()
	today := time.Now().UTC().Format("2006-01-02")

	// На New Releases 2 игры
	scraperMock.On("FetchNewReleases", mock.Anything).Return([]string{"game-1", "game-2"}, nil).Once()

	g1, r1 := sampleGame("game-1")
	g2, r2 := sampleGame("game-2")
	scraperMock.On("FetchGameDetails", mock.Anything, "game-1").Return(g1, r1, nil).Once()
	scraperMock.On("FetchGameDetails", mock.Anything, "game-2").Return(g2, r2, nil).Once()

	llmSummary := &llm.SummaryResult{CriticPros: "Visuals", CriticCons: "Bugs", UserPros: "Fun", UserCons: "Hard"}
	llmMock.On("SummarizeReviews", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(llmSummary, nil).Twice()
	llmMock.On("GetEmbedding", mock.Anything, mock.Anything).Return([]float32{0.1, 0.2}, nil).Twice()

	// Запуск
	processed, err := mgr.ExecuteCycle(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, processed)

	// Проверяем фиксацию даты и состояния
	lastDate, err := db.GetState(ctx, "last_crawl_date")
	require.NoError(t, err)
	require.Equal(t, today, lastDate)

	nextPage, err := db.GetState(ctx, "current_page")
	require.NoError(t, err)
	require.Equal(t, "1", nextPage)

	// Проверяем, что обе игры помечены как обработанные сегодня
	p1, _ := db.IsProcessedOnDate(ctx, "game-1", today)
	p2, _ := db.IsProcessedOnDate(ctx, "game-2", today)
	require.True(t, p1)
	require.True(t, p2)
}

func TestWorker_SubsequentRun_StrictPaginationNoDofetch(t *testing.T) {
	db, scraperMock, llmMock, mgr := setupWorkerEnv(t)
	defer db.Close()

	ctx := context.Background()
	today := time.Now().UTC().Format("2006-01-02")

	// Устанавливаем, что сегодня уже был первый запуск
	_ = db.SetState(ctx, "last_crawl_date", today)
	_ = db.SetState(ctx, "current_page", "1")

	// И game-1 уже обрабатывалась сегодня
	_ = db.MarkProcessed(ctx, "game-1", today)

	// На странице 1 каталога находятся game-1 (уже обработана) и game-3 (новая)
	scraperMock.On("FetchBrowsePage", mock.Anything, 1).Return([]string{"game-1", "game-3"}, nil).Once()

	g3, r3 := sampleGame("game-3")
	scraperMock.On("FetchGameDetails", mock.Anything, "game-3").Return(g3, r3, nil).Once()

	llmSummary := &llm.SummaryResult{CriticPros: "P", CriticCons: "C", UserPros: "UP", UserCons: "UC"}
	llmMock.On("SummarizeReviews", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(llmSummary, nil).Once()
	llmMock.On("GetEmbedding", mock.Anything, mock.Anything).Return([]float32{0.1, 0.2}, nil).Once()

	// Запуск
	processed, err := mgr.ExecuteCycle(ctx)
	require.NoError(t, err)
	// Должна быть обработана ТОЛЬКО game-3 (без добора со страницы 2!)
	require.Equal(t, 1, processed)

	// current_page должен стать 2
	nextPage, err := db.GetState(ctx, "current_page")
	require.NoError(t, err)
	require.Equal(t, "2", nextPage)
}

func TestWorker_DayRolloverReset(t *testing.T) {
	db, scraperMock, llmMock, mgr := setupWorkerEnv(t)
	defer db.Close()

	ctx := context.Background()
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	// Вчера были на странице 10
	_ = db.SetState(ctx, "last_crawl_date", yesterday)
	_ = db.SetState(ctx, "current_page", "10")

	// Сегодня новый день -> должен сброситься на New Releases!
	scraperMock.On("FetchNewReleases", mock.Anything).Return([]string{"new-game-today"}, nil).Once()

	g, r := sampleGame("new-game-today")
	scraperMock.On("FetchGameDetails", mock.Anything, "new-game-today").Return(g, r, nil).Once()

	llmSummary := &llm.SummaryResult{}
	llmMock.On("SummarizeReviews", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(llmSummary, nil).Once()
	llmMock.On("GetEmbedding", mock.Anything, mock.Anything).Return([]float32{0.1, 0.2}, nil).Once()

	processed, err := mgr.ExecuteCycle(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, processed)

	// Страница сбросилась на 1 для последующих вызовов
	page, err := db.GetState(ctx, "current_page")
	require.NoError(t, err)
	require.Equal(t, "1", page)
}

func TestWorker_ForcedModes(t *testing.T) {
	db, scraperMock, llmMock, mgr := setupWorkerEnv(t)
	defer db.Close()

	ctx := context.Background()

	// 1. Принудительный запуск кастомной страницы 5
	scraperMock.On("FetchBrowsePage", mock.Anything, 5).Return([]string{"game-on-page-5"}, nil).Once()
	g5, r5 := sampleGame("game-on-page-5")
	scraperMock.On("FetchGameDetails", mock.Anything, "game-on-page-5").Return(g5, r5, nil).Once()

	llmSummary := &llm.SummaryResult{}
	llmMock.On("SummarizeReviews", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(llmSummary, nil).Once()
	llmMock.On("GetEmbedding", mock.Anything, mock.Anything).Return([]float32{0.1, 0.2}, nil).Once()

	processed, err := mgr.ExecuteMode(ctx, worker.RunModeCustomPage, 5)
	require.NoError(t, err)
	require.Equal(t, 1, processed)

	// 2. Принудительный запуск New Releases
	scraperMock.On("FetchNewReleases", mock.Anything).Return([]string{"forced-new-rel"}, nil).Once()
	gN, rN := sampleGame("forced-new-rel")
	scraperMock.On("FetchGameDetails", mock.Anything, "forced-new-rel").Return(gN, rN, nil).Once()

	llmMock.On("SummarizeReviews", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(llmSummary, nil).Once()
	llmMock.On("GetEmbedding", mock.Anything, mock.Anything).Return([]float32{0.1, 0.2}, nil).Once()

	processed, err = mgr.ExecuteMode(ctx, worker.RunModeNewReleases, 0)
	require.NoError(t, err)
	require.Equal(t, 1, processed)
}
