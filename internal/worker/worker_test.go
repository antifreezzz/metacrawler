package worker_test

import (
	"context"
	"fmt"
	"sync"
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

func (m *MockLLM) TranslateToRussian(ctx context.Context, text string) (string, error) {
	// Перевод стал частью обычного цикла, поэтому по умолчанию возвращаем
	// детерминированный результат, не требуя явного expectation в каждом тесте.
	for _, call := range m.ExpectedCalls {
		if call.Method == "TranslateToRussian" {
			args := m.Called(ctx, text)
			return args.String(0), args.Error(1)
		}
	}
	return "RU:" + text, nil
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
		{ReviewType: domain.ReviewTypeCritic, Author: "Critic1", Text: "Good game", Platform: "pc"},
		{ReviewType: domain.ReviewTypeUser, Author: "User1", Text: "Awesome", Platform: "pc"},
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

// TestWorker_ConcurrentRunsRejected проверяет атомарный глобальный lock:
// второй цикл не должен стартовать, пока идет первый.
func TestWorker_ConcurrentRunsRejected(t *testing.T) {
	db, scraperMock, _, mgr := setupWorkerEnv(t)
	defer db.Close()

	ctx := context.Background()
	gate := make(chan struct{})
	started := make(chan struct{})

	scraperMock.On("FetchNewReleases", mock.Anything).Run(func(mock.Arguments) {
		close(started)
		<-gate
	}).Return([]string{}, nil).Once()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = mgr.ExecuteCycle(ctx)
	}()

	<-started
	_, err := mgr.ExecuteCycle(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already running")
	require.True(t, mgr.IsRunning())

	close(gate)
	wg.Wait()
	require.False(t, mgr.IsRunning())
}

// TestWorker_PageNotAdvancedOnProcessingError: при ошибке обработки игры
// курсор страницы не продвигается, чтобы пакет был перечитан.
func TestWorker_PageNotAdvancedOnProcessingError(t *testing.T) {
	db, scraperMock, _, mgr := setupWorkerEnv(t)
	defer db.Close()

	ctx := context.Background()
	today := domain.Now().Format("2006-01-02")
	_ = db.SetState(ctx, "last_crawl_date", today)
	_ = db.SetState(ctx, "current_page", "3")

	scraperMock.On("FetchBrowsePage", mock.Anything, 3).Return([]string{"bad-game"}, nil).Once()
	scraperMock.On("FetchGameDetails", mock.Anything, "bad-game").
		Return((*domain.Game)(nil), []domain.Review(nil), fmt.Errorf("boom")).Once()

	processed, err := mgr.ExecuteCycle(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, processed)

	page, _ := db.GetState(ctx, "current_page")
	require.Equal(t, "3", page, "курсор не должен продвигаться при ошибке обработки")
}

// TestWorker_NewReleasesCursorNotAdvancedOnError: сбой в первом запуске суток
// не фиксирует дату, поэтому пакет будет перечитан.
func TestWorker_NewReleasesCursorNotAdvancedOnError(t *testing.T) {
	db, scraperMock, _, mgr := setupWorkerEnv(t)
	defer db.Close()

	ctx := context.Background()
	today := domain.Now().Format("2006-01-02")

	scraperMock.On("FetchNewReleases", mock.Anything).Return([]string{"bad-game"}, nil).Once()
	scraperMock.On("FetchGameDetails", mock.Anything, "bad-game").
		Return((*domain.Game)(nil), []domain.Review(nil), fmt.Errorf("boom")).Once()

	processed, err := mgr.ExecuteCycle(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, processed)

	lastDate, _ := db.GetState(ctx, "last_crawl_date")
	require.NotEqual(t, today, lastDate, "дата не должна фиксироваться при сбое обработки")
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

	_, err := mgr.RecrawlGame(ctx, "attrib-game", worker.AllRecrawlOptions())
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

	// Отзыв без установленной платформы не сохраняется ни на одну платформу
	// (строгая платформенная привязка, без копирования на все).
	require.ElementsMatch(t, []string{"CriticPC"}, byPlatform["pc"])
	require.ElementsMatch(t, []string{"CriticPS5"}, byPlatform["playstation-5"])
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

	saved, err := mgr.RecrawlGame(ctx, "llm-error-game", worker.AllRecrawlOptions())
	// Ошибка LLM не должна валить сохранение игры
	require.NoError(t, err)
	require.NotNil(t, saved)

	summary, err := db.GetPlatformSummary(ctx, saved.Platforms[0].ID)
	require.NoError(t, err)
	require.Nil(t, summary, "выдуманное резюме недопустимо: строки summary быть не должно")
}

// recordingLLM - фейковый LLM со счётчиком вызовов и пиковой одновременности.
type recordingLLM struct {
	mu             sync.Mutex
	summarizeCalls int
	translateCalls int
	peakConcurrent int
	current        int
	delay          time.Duration
}

func (m *recordingLLM) SummarizeReviews(ctx context.Context, title, platform string, critics, users []domain.Review) (*llm.SummaryResult, error) {
	m.mu.Lock()
	m.summarizeCalls++
	m.current++
	if m.current > m.peakConcurrent {
		m.peakConcurrent = m.current
	}
	m.mu.Unlock()

	if m.delay > 0 {
		time.Sleep(m.delay)
	}

	m.mu.Lock()
	m.current--
	m.mu.Unlock()

	return &llm.SummaryResult{CriticPros: "P:" + platform}, nil
}

func (m *recordingLLM) GetEmbedding(ctx context.Context, text string) ([]float32, error) {
	return []float32{0.1}, nil
}

func (m *recordingLLM) TranslateToRussian(ctx context.Context, text string) (string, error) {
	m.mu.Lock()
	m.translateCalls++
	m.mu.Unlock()
	return "RU:" + text, nil
}

func (m *recordingLLM) HasAPIKey() bool        { return true }
func (m *recordingLLM) ChatModel() string      { return "test-model" }
func (m *recordingLLM) EmbeddingModel() string { return "test-emb" }

func setupWorkerEnvWithLLM(t *testing.T, llmClient worker.LLMClient) (*storage.DB, *MockScraper, *worker.Manager) {
	t.Helper()
	db, err := storage.New(":memory:")
	require.NoError(t, err)

	cfg := &config.Config{
		CrawlDelayMinMs: 1,
		CrawlDelayMaxMs: 2,
	}
	scraperMock := new(MockScraper)
	mgr := worker.NewManager(db, scraperMock, llmClient, nil, cfg)
	return db, scraperMock, mgr
}

func TestParseRecrawlOptions(t *testing.T) {
	cases := []struct {
		raw  string
		want worker.RecrawlOptions
	}{
		{"", worker.AllRecrawlOptions()},
		{"all", worker.AllRecrawlOptions()},
		{"scrape,summaries,youtube,embedding,translation", worker.AllRecrawlOptions()},
		{"scrape", worker.RecrawlOptions{Scrape: true}},
		{"summaries", worker.RecrawlOptions{Summaries: true}},
		{"youtube", worker.RecrawlOptions{YouTube: true}},
		{"embedding", worker.RecrawlOptions{Embedding: true}},
		{"translation", worker.RecrawlOptions{Translation: true}},
		{"translate", worker.RecrawlOptions{Translation: true}},
		{" summary , youtube ", worker.RecrawlOptions{Summaries: true, YouTube: true}},
		{"unknown", worker.RecrawlOptions{}},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, worker.ParseRecrawlOptions(tc.raw), "raw=%q", tc.raw)
	}
}

func TestWorker_CycleSkipsSummaryWhenNoNewReviews(t *testing.T) {
	recLLM := &recordingLLM{}
	db, scraperMock, mgr := setupWorkerEnvWithLLM(t, recLLM)
	defer db.Close()

	ctx := context.Background()

	score80 := 80
	game := &domain.Game{
		Slug:  "no-new-game",
		Title: "No New Game",
		Platforms: []domain.GamePlatform{
			{Platform: "pc", Metascore: &score80},
		},
	}
	revs := []domain.Review{
		{ReviewType: domain.ReviewTypeCritic, Author: "C1", Text: "First review"},
	}

	// Предзаполняем игру, отзывы и резюме в БД (без пометки "обработано").
	require.NoError(t, db.UpsertGame(ctx, game))
	saved, err := db.GetGameBySlug(ctx, "no-new-game")
	require.NoError(t, err)
	platID := saved.Platforms[0].ID
	for i := range revs {
		revs[i].GamePlatformID = platID
	}
	savedCount, err := db.SaveReviews(ctx, revs)
	require.NoError(t, err)
	require.Equal(t, 1, savedCount)
	require.NoError(t, db.UpsertPlatformSummary(ctx, &domain.PlatformSummary{
		GamePlatformID: platID,
		CriticPros:     "existing",
	}))

	// Автоматический цикл: отзывы те же, резюме уже есть -> LLM не вызывается.
	scraperMock.On("FetchNewReleases", mock.Anything).Return([]string{"no-new-game"}, nil).Once()
	scraperMock.On("FetchGameDetails", mock.Anything, "no-new-game").Return(game, revs, nil).Once()

	processed, err := mgr.ExecuteCycle(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, processed)
	require.Equal(t, 0, recLLM.summarizeCalls,
		"резюме не должно пересобираться в цикле, если отзывы не изменились и резюме уже есть")
}

func TestWorker_RecrawlForcesSummaryRegen(t *testing.T) {
	recLLM := &recordingLLM{}
	db, scraperMock, mgr := setupWorkerEnvWithLLM(t, recLLM)
	defer db.Close()

	ctx := context.Background()

	score80 := 80
	game := &domain.Game{
		Slug:  "force-game",
		Title: "Force Game",
		Platforms: []domain.GamePlatform{
			{Platform: "pc", Metascore: &score80},
		},
	}
	revs := []domain.Review{
		{ReviewType: domain.ReviewTypeCritic, Author: "C1", Text: "First review", Platform: "pc"},
	}
	scraperMock.On("FetchGameDetails", mock.Anything, "force-game").Return(game, revs, nil).Once()

	saved, err := mgr.RecrawlGame(ctx, "force-game", worker.AllRecrawlOptions())
	require.NoError(t, err)
	require.Equal(t, 1, recLLM.summarizeCalls)
	require.NotEmpty(t, saved.Platforms[0].Reviews, "возвращаемая игра должна содержать отзывы")

	// Принудительный пересбор только резюме: отзывы не изменились, но резюме регенерируется,
	// а Metacritic повторно не скрейпится.
	saved, err = mgr.RecrawlGame(ctx, "force-game", worker.RecrawlOptions{Summaries: true})
	require.NoError(t, err)
	require.Equal(t, 2, recLLM.summarizeCalls,
		"force-пересбор должен регенерировать резюме даже без новых отзывов")
	require.NotEmpty(t, saved.Platforms[0].Reviews)
}

func TestWorker_TranslatesDescriptionOnCycle(t *testing.T) {
	recLLM := &recordingLLM{}
	db, scraperMock, mgr := setupWorkerEnvWithLLM(t, recLLM)
	defer db.Close()

	ctx := context.Background()
	game, reviews := sampleGame("translate-game")
	scraperMock.On("FetchNewReleases", mock.Anything).Return([]string{"translate-game"}, nil).Once()
	scraperMock.On("FetchGameDetails", mock.Anything, "translate-game").Return(game, reviews, nil).Once()

	processed, err := mgr.ExecuteCycle(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, processed)
	require.Equal(t, 1, recLLM.translateCalls, "описание должно быть переведено в обычном цикле")

	saved, err := db.GetGameBySlug(ctx, "translate-game")
	require.NoError(t, err)
	require.Equal(t, "RU:Description of translate-game", saved.DescriptionRU)
}

func TestWorker_CycleSkipsTranslationWhenPresent(t *testing.T) {
	recLLM := &recordingLLM{}
	db, scraperMock, mgr := setupWorkerEnvWithLLM(t, recLLM)
	defer db.Close()

	ctx := context.Background()
	game, reviews := sampleGame("already-translated")
	require.NoError(t, db.UpsertGame(ctx, game))
	saved, err := db.GetGameBySlug(ctx, "already-translated")
	require.NoError(t, err)
	require.NoError(t, db.SaveGameTranslation(ctx, saved.ID, "Готовый перевод"))

	scraperMock.On("FetchNewReleases", mock.Anything).Return([]string{"already-translated"}, nil).Once()
	scraperMock.On("FetchGameDetails", mock.Anything, "already-translated").Return(game, reviews, nil).Once()

	processed, err := mgr.ExecuteCycle(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, processed)
	require.Equal(t, 0, recLLM.translateCalls,
		"перевод не должен пересобираться, если описание не изменилось и перевод уже есть")

	final, err := db.GetGameBySlug(ctx, "already-translated")
	require.NoError(t, err)
	require.Equal(t, "Готовый перевод", final.DescriptionRU)
}

func TestWorker_RecrawlForcesTranslationRegen(t *testing.T) {
	recLLM := &recordingLLM{}
	db, scraperMock, mgr := setupWorkerEnvWithLLM(t, recLLM)
	defer db.Close()

	ctx := context.Background()
	game, _ := sampleGame("force-translate")
	require.NoError(t, db.UpsertGame(ctx, game))
	saved, err := db.GetGameBySlug(ctx, "force-translate")
	require.NoError(t, err)
	require.NoError(t, db.SaveGameTranslation(ctx, saved.ID, "Старый перевод"))

	// force-пересбор только перевода: скрейпер не вызывается, перевод перезаписывается.
	updated, err := mgr.RecrawlGame(ctx, "force-translate", worker.RecrawlOptions{Translation: true})
	require.NoError(t, err)
	require.Equal(t, 1, recLLM.translateCalls)
	require.Equal(t, "RU:Description of force-translate", updated.DescriptionRU)
	scraperMock.AssertNotCalled(t, "FetchGameDetails", mock.Anything, mock.Anything)
}

func TestWorker_CycleSkipsYouTubeWhenAnalysisExists(t *testing.T) {
	recLLM := &slowingEmbedLLM{}
	recYT := &recordingYT{}

	db, err := storage.New(":memory:")
	require.NoError(t, err)
	defer db.Close()

	cfg := &config.Config{CrawlDelayMinMs: 1, CrawlDelayMaxMs: 2}
	scraperMock := new(MockScraper)
	mgr := worker.NewManager(db, scraperMock, recLLM, recYT, cfg)

	ctx := context.Background()

	game := &domain.Game{
		Slug:        "yt-skip-game",
		Title:       "YT Skip Game",
		Description: "desc",
		Platforms:   []domain.GamePlatform{{Platform: "pc"}},
	}
	require.NoError(t, db.UpsertGame(ctx, game))
	saved, err := db.GetGameBySlug(ctx, "yt-skip-game")
	require.NoError(t, err)
	require.NoError(t, db.UpsertYouTubeAnalysis(ctx, &domain.YouTubeAnalysis{
		GameID:  saved.ID,
		VideoID: "existing",
		Summary: "already there",
	}))

	scraperMock.On("FetchNewReleases", mock.Anything).Return([]string{"yt-skip-game"}, nil).Once()
	scraperMock.On("FetchGameDetails", mock.Anything, "yt-skip-game").Return(game, []domain.Review{}, nil).Once()

	processed, err := mgr.ExecuteCycle(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, processed)
	require.Equal(t, 0, recYT.calls, "цикл не должен перезапускать YouTube, если анализ уже есть")
}

func TestWorker_CycleSkipsSlugBeingRecrawled(t *testing.T) {
	db, scraperMock, llmMock, mgr := setupWorkerEnv(t)
	defer db.Close()

	ctx := context.Background()

	g, revs := sampleGame("busy-game")
	startedCh := make(chan struct{})
	release := make(chan struct{})

	scraperMock.On("FetchGameDetails", mock.Anything, "busy-game").Run(func(mock.Arguments) {
		close(startedCh)
		<-release
	}).Return(g, revs, nil).Once()

	llmMock.On("SummarizeReviews", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&llm.SummaryResult{}, nil).Maybe()
	llmMock.On("GetEmbedding", mock.Anything, mock.Anything).Return([]float32{0.1}, nil).Maybe()

	started, err := mgr.RecrawlGameAsync("busy-game", worker.AllRecrawlOptions())
	require.NoError(t, err)
	require.True(t, started)
	<-startedCh

	// Цикл видит ту же игру, но она уже обрабатывается -> пропуск, второго скрейпа нет.
	scraperMock.On("FetchNewReleases", mock.Anything).Return([]string{"busy-game"}, nil).Once()
	processed, err := mgr.ExecuteCycle(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, processed)

	close(release)
	require.Eventually(t, func() bool {
		return !mgr.IsRecrawling("busy-game")
	}, 2*time.Second, 10*time.Millisecond)
}

func TestWorker_ParallelPlatformSummaries(t *testing.T) {
	recLLM := &recordingLLM{delay: 50 * time.Millisecond}
	db, scraperMock, mgr := setupWorkerEnvWithLLM(t, recLLM)
	defer db.Close()

	ctx := context.Background()

	score := 80
	game := &domain.Game{
		Slug:  "parallel-game",
		Title: "Parallel Game",
		Platforms: []domain.GamePlatform{
			{Platform: "pc", Metascore: &score},
			{Platform: "playstation-5", Metascore: &score},
			{Platform: "xbox-series-x", Metascore: &score},
		},
	}
	revs := []domain.Review{
		{ReviewType: domain.ReviewTypeCritic, Author: "C1", Text: "Review one", Platform: "pc"},
		{ReviewType: domain.ReviewTypeCritic, Author: "C2", Text: "Review two", Platform: "playstation-5"},
		{ReviewType: domain.ReviewTypeCritic, Author: "C3", Text: "Review three", Platform: "xbox-series-x"},
	}
	scraperMock.On("FetchGameDetails", mock.Anything, "parallel-game").Return(game, revs, nil).Once()

	_, err := mgr.RecrawlGame(ctx, "parallel-game", worker.AllRecrawlOptions())
	require.NoError(t, err)

	// Все три платформы получили резюме, каждое - из своей платформы
	saved, err := db.GetGameBySlug(ctx, "parallel-game")
	require.NoError(t, err)
	require.Len(t, saved.Platforms, 3)
	for _, p := range saved.Platforms {
		s, err := db.GetPlatformSummary(ctx, p.ID)
		require.NoError(t, err)
		require.NotNil(t, s)
		require.Equal(t, "P:"+p.Platform, s.CriticPros)
	}

	// Платформенный анализ идет параллельно: наблюдалась одновременность >= 2
	require.GreaterOrEqual(t, recLLM.peakConcurrent, 2)
}

// slowingEmbedLLM - LLM с замедленной эмбеддинг-стадией для проверки параллельности.
type slowingEmbedLLM struct{}

func (m *slowingEmbedLLM) SummarizeReviews(ctx context.Context, title, platform string, critics, users []domain.Review) (*llm.SummaryResult, error) {
	return &llm.SummaryResult{}, nil
}

func (m *slowingEmbedLLM) GetEmbedding(ctx context.Context, text string) ([]float32, error) {
	time.Sleep(60 * time.Millisecond)
	return []float32{0.1, 0.2}, nil
}

func (m *slowingEmbedLLM) TranslateToRussian(ctx context.Context, text string) (string, error) {
	return "RU:" + text, nil
}

func (m *slowingEmbedLLM) HasAPIKey() bool        { return false }
func (m *slowingEmbedLLM) ChatModel() string      { return "" }
func (m *slowingEmbedLLM) EmbeddingModel() string { return "" }

// recordingYT - фейковый YouTube-клиент с трекингом одновременности.
type recordingYT struct {
	mu    sync.Mutex
	calls int
}

func (y *recordingYT) AnalyzeVideo(ctx context.Context, gameID, gameTitle string) (*domain.YouTubeAnalysis, error) {
	y.mu.Lock()
	y.calls++
	y.mu.Unlock()
	time.Sleep(40 * time.Millisecond)
	return &domain.YouTubeAnalysis{GameID: gameID, VideoID: "vid", Summary: "ok"}, nil
}

func TestWorker_EmbeddingAndYouTubeRunConcurrently(t *testing.T) {
	recLLM := &slowingEmbedLLM{}
	recYT := &recordingYT{}

	db, err := storage.New(":memory:")
	require.NoError(t, err)
	defer db.Close()

	cfg := &config.Config{CrawlDelayMinMs: 1, CrawlDelayMaxMs: 2}
	scraperMock := new(MockScraper)
	mgr := worker.NewManager(db, scraperMock, recLLM, recYT, cfg)

	ctx := context.Background()

	game := &domain.Game{
		Slug:        "conc-game",
		Title:       "Conc Game",
		Description: "Description of conc game",
		Platforms: []domain.GamePlatform{
			{Platform: "pc"},
		},
	}
	revs := []domain.Review{}
	scraperMock.On("FetchGameDetails", mock.Anything, "conc-game").Return(game, revs, nil).Once()

	start := time.Now()
	_, err = mgr.RecrawlGame(ctx, "conc-game", worker.AllRecrawlOptions())
	require.NoError(t, err)
	elapsed := time.Since(start)

	// Замедленная эмбеддинг-стадия: 60мс, YouTube: 40мс.
	// Последовательно было бы >= 100мс, параллельно - около 60мс.
	require.Less(t, elapsed, 95*time.Millisecond,
		"embedding и YouTube должны выполняться параллельно (elapsed=%v)", elapsed)
	require.Equal(t, 1, recYT.calls)
}

func TestWorker_RecordsScoreHistory(t *testing.T) {
	db, scraperMock, llmMock, mgr := setupWorkerEnv(t)
	defer db.Close()

	ctx := context.Background()

	score80, score85 := 80, 85
	game1 := &domain.Game{
		Slug:  "scoring-game",
		Title: "Scoring Game",
		Platforms: []domain.GamePlatform{
			{Platform: "pc", Metascore: &score80},
		},
	}
	game2 := &domain.Game{
		Slug:  "scoring-game",
		Title: "Scoring Game",
		Platforms: []domain.GamePlatform{
			{Platform: "pc", Metascore: &score85},
		},
	}

	revs := []domain.Review{}
	scraperMock.On("FetchGameDetails", mock.Anything, "scoring-game").Return(game1, revs, nil).Once()
	llmMock.On("SummarizeReviews", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(&llm.SummaryResult{}, nil).Maybe()
	llmMock.On("GetEmbedding", mock.Anything, mock.Anything).Return([]float32{0.1}, nil).Once()

	_, err := mgr.RecrawlGame(ctx, "scoring-game", worker.AllRecrawlOptions())
	require.NoError(t, err)

	// Пересбор: metascore изменился с 80 на 85
	scraperMock.On("FetchGameDetails", mock.Anything, "scoring-game").Return(game2, revs, nil).Once()
	llmMock.On("GetEmbedding", mock.Anything, mock.Anything).Return([]float32{0.2}, nil).Once()

	_, err = mgr.RecrawlGame(ctx, "scoring-game", worker.AllRecrawlOptions())
	require.NoError(t, err)

	saved, err := db.GetGameBySlug(ctx, "scoring-game")
	require.NoError(t, err)

	hist, err := db.GetScoreHistory(ctx, saved.Platforms[0].ID)
	require.NoError(t, err)
	require.Len(t, hist, 2)
	require.Equal(t, 80, *hist[0].Metascore)
	require.Equal(t, 85, *hist[1].Metascore)
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

	savedGame, err := mgr.RecrawlGame(ctx, "recrawl-target", worker.AllRecrawlOptions())
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
	started, err := mgr.RecrawlGameAsync("async-target", worker.AllRecrawlOptions())
	require.NoError(t, err)
	require.True(t, started)

	// Ждем, пока горутина начнет выполнение
	<-startedCh

	// Статус во время работы - running
	state, ok := mgr.RecrawlStatus("async-target")
	require.True(t, ok)
	require.Equal(t, "running", state.Status)
	require.Equal(t, "scrape,summaries,youtube,embedding,translation", state.Options)

	// 2. Повторный запуск для того же slug должен вернуть false (already active)
	started2, err2 := mgr.RecrawlGameAsync("async-target", worker.AllRecrawlOptions())
	require.NoError(t, err2)
	require.False(t, started2)
	require.True(t, mgr.IsRecrawling("async-target"))

	// Пустой набор частей недопустим
	_, errEmpty := mgr.RecrawlGameAsync("other-target", worker.RecrawlOptions{})
	require.Error(t, errEmpty)

	// Разрешаем завершиться
	close(continueCh)

	// Ждем пока освободится
	require.Eventually(t, func() bool {
		return !mgr.IsRecrawling("async-target")
	}, 2*time.Second, 10*time.Millisecond)

	// Статус после успешного завершения - done
	finalState, ok := mgr.RecrawlStatus("async-target")
	require.True(t, ok)
	require.Equal(t, "done", finalState.Status)
	require.Empty(t, finalState.Error)
}

func TestWorker_RecrawlStatusError(t *testing.T) {
	db, scraperMock, _, mgr := setupWorkerEnv(t)
	defer db.Close()

	scraperMock.On("FetchGameDetails", mock.Anything, "broken-game").
		Return((*domain.Game)(nil), []domain.Review(nil), fmt.Errorf("scrape failed")).Once()

	started, err := mgr.RecrawlGameAsync("broken-game", worker.RecrawlOptions{Scrape: true})
	require.NoError(t, err)
	require.True(t, started)

	require.Eventually(t, func() bool {
		state, ok := mgr.RecrawlStatus("broken-game")
		return ok && state.Status == "error"
	}, 2*time.Second, 10*time.Millisecond)

	state, _ := mgr.RecrawlStatus("broken-game")
	require.Contains(t, state.Error, "scrape failed")
}
