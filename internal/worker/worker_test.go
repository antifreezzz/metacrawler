package worker_test

import (
	"context"
	"fmt"
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

func (m *MockLLM) HasAPIKey() bool {
	return false
}

func (m *MockLLM) ChatModel() string {
	return "gpt-4o-mini"
}

func (m *MockLLM) EmbeddingModel() string {
	return "text-embedding-3-small"
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
	today := domain.Now().Format("2006-01-02")

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
	today := domain.Now().Format("2006-01-02")

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
	yesterday := domain.Now().AddDate(0, 0, -1).Format("2006-01-02")

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

func TestWorker_ReviewPlatformAttribution(t *testing.T) {
	db, scraperMock, llmMock, mgr := setupWorkerEnv(t)
	defer db.Close()

	ctx := context.Background()

	score80 := 80
	game := &domain.Game{
		Slug:  "attrib-game",
		Title: "Attrib Game",
		Platforms: []domain.GamePlatform{
			{Platform: "pc", Metascore: &score80},
			{Platform: "playstation-5", Metascore: &score80},
		},
	}
	reviews := []domain.Review{
		{ReviewType: domain.ReviewTypeCritic, Author: "CriticPC", Text: "PC review", Platform: "pc"},
		{ReviewType: domain.ReviewTypeCritic, Author: "CriticPS5", Text: "PS5 review", Platform: "playstation-5"},
		{ReviewType: domain.ReviewTypeCritic, Author: "CriticAny", Text: "Any platform review"},
	}
	scraperMock.On("FetchGameDetails", mock.Anything, "attrib-game").Return(game, reviews, nil).Once()
	llmMock.On("SummarizeReviews", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(&llm.SummaryResult{}, nil).Maybe()
	llmMock.On("GetEmbedding", mock.Anything, mock.Anything).Return([]float32{0.1}, nil).Maybe()

	_, err := mgr.RecrawlGame(ctx, "attrib-game")
	require.NoError(t, err)

	saved, err := db.GetGameBySlug(ctx, "attrib-game")
	require.NoError(t, err)
	require.Len(t, saved.Platforms, 2)

	byPlatform := make(map[string][]string)
	for _, p := range saved.Platforms {
		revs, err := db.GetReviewsByPlatformID(ctx, p.ID)
		require.NoError(t, err)
		authors := make([]string, 0, len(revs))
		for _, r := range revs {
			authors = append(authors, r.Author)
		}
		byPlatform[p.Platform] = authors
	}

	// Отзыв с платформой должен попасть только в свою платформу,
	// отзыв без платформы - во все (обратная совместимость).
	require.ElementsMatch(t, []string{"CriticPC", "CriticAny"}, byPlatform["pc"])
	require.ElementsMatch(t, []string{"CriticPS5", "CriticAny"}, byPlatform["playstation-5"])
}

func TestWorker_LLMError_NoSummaryStored(t *testing.T) {
	db, scraperMock, llmMock, mgr := setupWorkerEnv(t)
	defer db.Close()

	ctx := context.Background()

	g, revs := sampleGame("llm-error-game")
	scraperMock.On("FetchGameDetails", mock.Anything, "llm-error-game").Return(g, revs, nil).Once()
	llmMock.On("SummarizeReviews", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return((*llm.SummaryResult)(nil), fmt.Errorf("%w: no key", llm.ErrLLMUnavailable)).Once()
	llmMock.On("GetEmbedding", mock.Anything, mock.Anything).Return([]float32{0.1}, nil).Once()

	saved, err := mgr.RecrawlGame(ctx, "llm-error-game")
	// Ошибка LLM не должна валить сохранение игры
	require.NoError(t, err)
	require.NotNil(t, saved)

	summary, err := db.GetPlatformSummary(ctx, saved.Platforms[0].ID)
	require.NoError(t, err)
	require.Nil(t, summary, "выдуманное резюме недопустимо: строки summary быть не должно")
}

func TestWorker_RecrawlGame(t *testing.T) {
	db, scraperMock, llmMock, mgr := setupWorkerEnv(t)
	defer db.Close()

	ctx := context.Background()

	g, revs := sampleGame("recrawl-target")
	scraperMock.On("FetchGameDetails", mock.Anything, "recrawl-target").Return(g, revs, nil).Once()

	llmSummary := &llm.SummaryResult{
		CriticPros: "Awesome graphics",
		UserPros:   "Fun gameplay",
	}
	llmMock.On("SummarizeReviews", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(llmSummary, nil).Once()
	llmMock.On("GetEmbedding", mock.Anything, mock.Anything).Return([]float32{0.5, 0.5}, nil).Once()

	savedGame, err := mgr.RecrawlGame(ctx, "recrawl-target")
	require.NoError(t, err)
	require.NotNil(t, savedGame)
	require.Equal(t, "recrawl-target", savedGame.Slug)

	// Проверяем, что резюме сохранено
	plat := savedGame.Platforms[0]
	summary, err := db.GetPlatformSummary(ctx, plat.ID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.Equal(t, "Awesome graphics", summary.CriticPros)
}

func TestWorker_RecrawlGameAsync(t *testing.T) {
	db, scraperMock, llmMock, mgr := setupWorkerEnv(t)
	defer db.Close()

	g, revs := sampleGame("async-target")
	startedCh := make(chan struct{})
	continueCh := make(chan struct{})

	scraperMock.On("FetchGameDetails", mock.Anything, "async-target").Run(func(args mock.Arguments) {
		close(startedCh)
		<-continueCh
	}).Return(g, revs, nil).Once()

	llmSummary := &llm.SummaryResult{
		CriticPros: "Great",
	}
	llmMock.On("SummarizeReviews", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(llmSummary, nil).Maybe()
	llmMock.On("GetEmbedding", mock.Anything, mock.Anything).Return([]float32{0.5, 0.5}, nil).Maybe()

	// 1. Первый запуск должен успешно стартовать
	started, err := mgr.RecrawlGameAsync("async-target")
	require.NoError(t, err)
	require.True(t, started)

	// Ждем, пока горутина начнет выполнение
	<-startedCh

	// 2. Повторный запуск для того же slug должен вернуть false (already active)
	started2, err2 := mgr.RecrawlGameAsync("async-target")
	require.NoError(t, err2)
	require.False(t, started2)
	require.True(t, mgr.IsRecrawling("async-target"))

	// Разрешаем завершиться
	close(continueCh)

	// Ждем пока освободится
	require.Eventually(t, func() bool {
		return !mgr.IsRecrawling("async-target")
	}, 2*time.Second, 10*time.Millisecond)
}

