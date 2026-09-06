package worker

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"metacrawler/internal/config"
	"metacrawler/internal/domain"
	"metacrawler/internal/llm"
	"metacrawler/internal/storage"
)

type ScraperClient interface {
	FetchNewReleases(ctx context.Context) ([]string, error)
	FetchBrowsePage(ctx context.Context, page int) ([]string, error)
	FetchGameDetails(ctx context.Context, slug string) (*domain.Game, []domain.Review, error)
}

type LLMClient interface {
	SummarizeReviews(ctx context.Context, title, platform string, critics, users []domain.Review) (*llm.SummaryResult, error)
	GetEmbedding(ctx context.Context, text string) ([]float32, error)
	HasAPIKey() bool
	ChatModel() string
	EmbeddingModel() string
}

type YouTubeClient interface {
	AnalyzeVideo(ctx context.Context, gameID, gameTitle string) (*domain.YouTubeAnalysis, error)
}

type RunMode string

const (
	RunModeAuto        RunMode = "auto"
	RunModeNewReleases RunMode = "new_releases"
	RunModeNextPage    RunMode = "next_page"
	RunModeCustomPage  RunMode = "custom_page"
)

type StatusInfo struct {
	Status         string    `json:"status"` // Idle, Running, Error
	CurrentTask    string    `json:"current_task"`
	ProcessedCount int       `json:"processed_count"`
	TotalInBatch   int       `json:"total_in_batch"`
	CurrentIndex   int       `json:"current_index"`
	LastRunAt      time.Time `json:"last_run_at"`
	LastError      string    `json:"last_error"`
	CurrentPage    string    `json:"current_page"`
	Logs           []string  `json:"logs"`
}

type Manager struct {
	db      *storage.DB
	scraper ScraperClient
	llm     LLMClient
	youtube YouTubeClient
	cfg     *config.Config
	cron    *cron.Cron

	mu             sync.RWMutex
	status         string
	currentTask    string
	processedCount int
	totalInBatch   int
	currentIndex   int
	lastRunAt      time.Time
	lastError      string
	logs           []string

	subscribers   map[chan StatusInfo]struct{}
	subscribersMu sync.RWMutex
}

func NewManager(db *storage.DB, scraper ScraperClient, llmClient LLMClient, ytClient YouTubeClient, cfg *config.Config) *Manager {
	return &Manager{
		db:          db,
		scraper:     scraper,
		llm:         llmClient,
		youtube:     ytClient,
		cfg:         cfg,
		status:      "Idle",
		subscribers: make(map[chan StatusInfo]struct{}),
		logs:        []string{"[Система] Воркер инициализирован и готов к работе."},
	}
}

func (m *Manager) addLog(msg string) {
	timeStr := domain.Now().Format("15:04:05")
	entry := fmt.Sprintf("[%s] %s", timeStr, msg)
	m.logs = append(m.logs, entry)
	if len(m.logs) > 300 {
		m.logs = m.logs[len(m.logs)-300:]
	}
}

func (m *Manager) StartCron() error {
	m.cron = cron.New(cron.WithLocation(domain.TimezoneUTC3))
	schedule := m.cfg.CronSchedule
	if schedule == "" {
		schedule = "0 * * * *"
	}

	_, err := m.cron.AddFunc(schedule, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		_, _ = m.ExecuteCycle(ctx)
	})
	if err != nil {
		return fmt.Errorf("add cron schedule %s: %w", schedule, err)
	}

	m.cron.Start()
	return nil
}

func (m *Manager) StopCron() {
	if m.cron != nil {
		m.cron.Stop()
	}
}

func (m *Manager) Subscribe() (chan StatusInfo, func()) {
	ch := make(chan StatusInfo, 20)
	m.subscribersMu.Lock()
	m.subscribers[ch] = struct{}{}
	m.subscribersMu.Unlock()

	// Сразу отправляем текущее состояние
	ch <- m.GetStatus()

	unsubscribe := func() {
		m.subscribersMu.Lock()
		delete(m.subscribers, ch)
		close(ch)
		m.subscribersMu.Unlock()
	}
	return ch, unsubscribe
}

func (m *Manager) notifySubscribers() {
	state := m.GetStatus()
	m.subscribersMu.RLock()
	defer m.subscribersMu.RUnlock()
	for ch := range m.subscribers {
		select {
		case ch <- state:
		default:
		}
	}
}

func (m *Manager) GetStatus() StatusInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	curPage, _ := m.db.GetState(context.Background(), "current_page")
	if curPage == "" {
		curPage = "1"
	}

	logsCopy := make([]string, len(m.logs))
	copy(logsCopy, m.logs)

	return StatusInfo{
		Status:         m.status,
		CurrentTask:    m.currentTask,
		ProcessedCount: m.processedCount,
		TotalInBatch:   m.totalInBatch,
		CurrentIndex:   m.currentIndex,
		LastRunAt:      m.lastRunAt,
		LastError:      m.lastError,
		CurrentPage:    curPage,
		Logs:           logsCopy,
	}
}

func (m *Manager) setRunning(task string, total int) {
	m.mu.Lock()
	m.status = "Running"
	m.currentTask = task
	m.totalInBatch = total
	m.currentIndex = 0
	m.lastError = ""
	m.addLog(task)
	m.mu.Unlock()
	m.notifySubscribers()
}

func (m *Manager) setProgress(current int, task string) {
	m.mu.Lock()
	m.currentIndex = current
	m.currentTask = task
	m.addLog(task)
	m.mu.Unlock()
	m.notifySubscribers()
}

func (m *Manager) setFinished(processed int, err error) {
	m.mu.Lock()
	m.lastRunAt = domain.Now()
	m.processedCount = processed
	if err != nil {
		m.status = "Error"
		m.lastError = err.Error()
		m.currentTask = fmt.Sprintf("Error: %v", err)
		m.addLog(fmt.Sprintf("❌ Ошибка выполнения: %v", err))
	} else {
		m.status = "Idle"
		m.currentTask = fmt.Sprintf("Finished. Processed %d games.", processed)
		m.addLog(fmt.Sprintf("🏁 Цикл завершён (UTC+3). Успешно обработано игр: %d.", processed))
	}
	m.mu.Unlock()
	m.notifySubscribers()
}

func (m *Manager) ExecuteCycle(ctx context.Context) (int, error) {
	return m.ExecuteMode(ctx, RunModeAuto, 0)
}

// ExecuteMode выполняет цикл в указанном режиме (авто суточный, принудительный new_releases, следующая страница или кастомная страница).
func (m *Manager) ExecuteMode(ctx context.Context, mode RunMode, customPage int) (int, error) {
	m.mu.Lock()
	if m.status == "Running" {
		m.mu.Unlock()
		return 0, fmt.Errorf("worker is already running")
	}
	m.mu.Unlock()

	today := domain.Now().Format("2006-01-02")
	lastCrawlDate, _ := m.db.GetState(ctx, "last_crawl_date")

	var candidateSlugs []string
	var err error

	useNewReleases := false
	if mode == RunModeNewReleases {
		useNewReleases = true
	} else if mode == RunModeAuto && lastCrawlDate != today {
		useNewReleases = true
	}

	if useNewReleases {
		m.setRunning("Запрос свежих релизов (New Releases)", 0)
		m.addLog("🚀 [Start] Режим: New Releases (свежие релизы Metacritic)")
		candidateSlugs, err = m.scraper.FetchNewReleases(ctx)
		if err != nil {
			m.setFinished(0, fmt.Errorf("fetch new releases: %w", err))
			return 0, err
		}
		m.addLog(fmt.Sprintf("📋 [Scraper] Получено игр из раздела New Releases: %d", len(candidateSlugs)))
		_ = m.db.SetState(ctx, "last_crawl_date", today)
		_ = m.db.SetState(ctx, "current_page", "1")
	} else {
		targetPage := 1
		if mode == RunModeCustomPage && customPage > 0 {
			targetPage = customPage
		} else {
			curPageStr, _ := m.db.GetState(ctx, "current_page")
			if p, err := strconv.Atoi(curPageStr); err == nil && p >= 1 {
				targetPage = p
			}
		}

		m.setRunning(fmt.Sprintf("Запрос каталога (Страница %d)", targetPage), 0)
		m.addLog(fmt.Sprintf("📄 [Start] Режим: Каталог (Страница %d)", targetPage))
		candidateSlugs, err = m.scraper.FetchBrowsePage(ctx, targetPage)
		if err != nil {
			m.setFinished(0, fmt.Errorf("fetch browse page %d: %w", targetPage, err))
			return 0, err
		}
		m.addLog(fmt.Sprintf("📋 [Scraper] Получено игр со страницы %d: %d", targetPage, len(candidateSlugs)))
		// Переход к следующей странице
		_ = m.db.SetState(ctx, "current_page", strconv.Itoa(targetPage+1))
	}

	// Отбираем только игры, которые сегодня ЕЩЕ НЕ обрабатывались (СТРОГО БЕЗ ДОБОРА со следующих страниц)
	var toProcess []string
	for _, slug := range candidateSlugs {
		processedToday, checkErr := m.db.IsProcessedOnDate(ctx, slug, today)
		if checkErr == nil && !processedToday {
			toProcess = append(toProcess, slug)
		}
	}

	skipped := len(candidateSlugs) - len(toProcess)
	m.addLog(fmt.Sprintf("🧹 [Filter] Дата проверки (UTC+3): %s | Новых для сбора: %d (пропущено ранее собранных: %d)", today, len(toProcess), skipped))

	if len(toProcess) == 0 {
		m.addLog("ℹ️ [Info] Все игры из этого списка уже собраны за сегодняшнюю дату. Переход в режим ожидания.")
		m.setFinished(0, nil)
		return 0, nil
	}

	m.setRunning(fmt.Sprintf("Processing %d games", len(toProcess)), len(toProcess))

	processedCount := 0
	for idx, slug := range toProcess {
		if ctx.Err() != nil {
			m.setFinished(processedCount, ctx.Err())
			return processedCount, ctx.Err()
		}

		// Кулдаун между действиями
		m.applyCooldown(ctx)

		m.setProgress(idx+1, fmt.Sprintf("Scraping game: %s (%d/%d)", slug, idx+1, len(toProcess)))
		m.addLog(fmt.Sprintf("🎮 [%d/%d] Обработка игры: %s", idx+1, len(toProcess), slug))

		_, err := m.processGame(ctx, slug, today)
		if err != nil {
			m.addLog(fmt.Sprintf("  ⚠️ [Worker] Ошибка обработки игры %s: %v (пропуск)", slug, err))
			continue
		}
		processedCount++
	}

	m.setFinished(processedCount, nil)
	return processedCount, nil
}

// RecrawlGame выполняет принудительный пересбор данных для одной конкретной игры.
func (m *Manager) RecrawlGame(ctx context.Context, slug string) (*domain.Game, error) {
	today := domain.Now().Format("2006-01-02")
	m.addLog(fmt.Sprintf("🔄 [Recrawl] Запущен принудительный пересбор для игры: %s", slug))
	return m.processGame(ctx, slug, today)
}

// BackfillMissingSummaries обходит игры в базе данных и догенерирует резюме для платформ, у которых есть отзывы, но нет резюме.
func (m *Manager) BackfillMissingSummaries(ctx context.Context) (int, error) {
	games, err := m.db.ListGames(ctx, storage.ListFilter{Limit: 1000})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, g := range games {
		for _, p := range g.Platforms {
			existing, _ := m.db.GetPlatformSummary(ctx, p.ID)
			if existing != nil {
				continue
			}
			reviews, _ := m.db.GetReviewsByPlatformID(ctx, p.ID)
			if len(reviews) == 0 {
				continue
			}
			var critics, users []domain.Review
			for _, r := range reviews {
				if r.ReviewType == domain.ReviewTypeCritic {
					critics = append(critics, r)
				} else {
					users = append(users, r)
				}
			}
			summary, llmErr := m.llm.SummarizeReviews(ctx, g.Title, p.Platform, critics, users)
			if llmErr == nil && summary != nil {
				_ = m.db.UpsertPlatformSummary(ctx, &domain.PlatformSummary{
					GamePlatformID: p.ID,
					CriticPros:     summary.CriticPros,
					CriticCons:     summary.CriticCons,
					UserPros:       summary.UserPros,
					UserCons:       summary.UserCons,
				})
				count++
				m.addLog(fmt.Sprintf("  🤖 [Backfill] Добавлено резюме для \"%s\" (%s)", g.Title, p.Platform))
			}
		}
	}
	return count, nil
}

func (m *Manager) processGame(ctx context.Context, slug, today string) (*domain.Game, error) {
	game, reviews, scrapeErr := m.scraper.FetchGameDetails(ctx, slug)
	if scrapeErr != nil {
		return nil, fmt.Errorf("fetch details: %w", scrapeErr)
	}
	if game == nil {
		return nil, fmt.Errorf("no game details found for %s", slug)
	}

	criticCount := 0
	userCount := 0
	for _, r := range reviews {
		if r.ReviewType == domain.ReviewTypeCritic {
			criticCount++
		} else {
			userCount++
		}
	}
	m.addLog(fmt.Sprintf("  ✅ [Scraper] \"%s\" загружена | Платформ: %d, Отзывов: %d (критики: %d, игроки: %d)", game.Title, len(game.Platforms), len(reviews), criticCount, userCount))

	// 1. Сохранение игры и её платформ
	if err := m.db.UpsertGame(ctx, game); err != nil {
		return nil, fmt.Errorf("upsert game: %w", err)
	}

	// Получаем сохраненную игру для актуальных platform IDs
	savedGame, err := m.db.GetGameBySlug(ctx, slug)
	if err != nil || savedGame == nil {
		return nil, fmt.Errorf("get saved game: %w", err)
	}

	// 2. Распределение отзывов по платформам и дедубликация
	for i, p := range savedGame.Platforms {
		var platReviews []domain.Review
		for _, r := range reviews {
			r.GamePlatformID = p.ID
			platReviews = append(platReviews, r)
		}
		savedCount, _ := m.db.SaveReviews(ctx, platReviews)

		// 3. Выборка всех отзывов по платформе (включая ранее сохраненные) для саммари
		allPlatReviews, _ := m.db.GetReviewsByPlatformID(ctx, p.ID)
		savedGame.Platforms[i].Reviews = allPlatReviews
		var critics, users []domain.Review
		for _, r := range allPlatReviews {
			if r.ReviewType == domain.ReviewTypeCritic {
				critics = append(critics, r)
			} else {
				users = append(users, r)
			}
		}

		// 4. Генерация резюме отзывов LLM
		if len(critics) > 0 || len(users) > 0 {
			summary, llmErr := m.llm.SummarizeReviews(ctx, savedGame.Title, p.Platform, critics, users)
			if llmErr != nil {
				m.addLog(fmt.Sprintf("  ⚠️ [LLM] Ошибка генерации резюме (%s): %v", p.Platform, llmErr))
			} else if summary != nil {
				platformSummary := &domain.PlatformSummary{
					GamePlatformID: p.ID,
					CriticPros:     summary.CriticPros,
					CriticCons:     summary.CriticCons,
					UserPros:       summary.UserPros,
					UserCons:       summary.UserCons,
				}
				_ = m.db.UpsertPlatformSummary(ctx, platformSummary)
				savedGame.Platforms[i].Summary = platformSummary
				if m.llm.HasAPIKey() {
					m.addLog(fmt.Sprintf("  🤖 [LLM] Резюме отзывов (%s) сгенерировано через %s", p.Platform, m.llm.ChatModel()))
				} else {
					m.addLog(fmt.Sprintf("  💡 [LLM] Резюме отзывов (%s) сформировано (локальный фоллбэк, LLM_API_KEY не задан)", p.Platform))
				}
			}
		} else {
			m.addLog(fmt.Sprintf("  ℹ️ [Reviews] Платформа %s: сохранено отзывов: %d", p.Platform, savedCount))
		}
	}

	// 5. Векторный эмбеддинг игры
	textForEmbedding := fmt.Sprintf("%s. %s. Developer: %s", savedGame.Title, savedGame.Description, savedGame.Developer)
	vec, embErr := m.llm.GetEmbedding(ctx, textForEmbedding)
	if embErr != nil {
		m.addLog(fmt.Sprintf("  ⚠️ [Embedding] Ошибка получения вектора: %v", embErr))
	} else if len(vec) > 0 {
		_ = m.db.SaveEmbedding(ctx, &domain.GameEmbedding{
			GameID:     savedGame.ID,
			Vector:     vec,
			Dimensions: len(vec),
		})
		if m.llm.HasAPIKey() {
			m.addLog(fmt.Sprintf("  ✨ [Embedding] Вектор получен через %s (%d dims)", m.llm.EmbeddingModel(), len(vec)))
		} else {
			m.addLog(fmt.Sprintf("  ✨ [Embedding] Вектор сгенерирован локально (детерминированный фоллбэк, %d dims)", len(vec)))
		}
	}

	// 6. YouTube
	if m.youtube != nil {
		ytAnalysis, ytErr := m.youtube.AnalyzeVideo(ctx, savedGame.ID, savedGame.Title)
		if ytErr != nil {
			m.addLog(fmt.Sprintf("  ⚠️ [YouTube] Летсплей не найден или ошибка: %v", ytErr))
		} else if ytAnalysis != nil {
			_ = m.db.UpsertYouTubeAnalysis(ctx, ytAnalysis)
		}
	}

	// 7. Помечаем игру как обработанную
	if today != "" {
		_ = m.db.MarkProcessed(ctx, slug, today)
		m.addLog(fmt.Sprintf("  💾 [DB] Игра \"%s\" успешно сохранена в базу на дату %s (UTC+3)", savedGame.Title, today))
	} else {
		m.addLog(fmt.Sprintf("  💾 [DB] Игра \"%s\" успешно сохранена в базу", savedGame.Title))
	}

	return savedGame, nil
}

func (m *Manager) applyCooldown(ctx context.Context) {
	minMs := m.cfg.CrawlDelayMinMs
	maxMs := m.cfg.CrawlDelayMaxMs
	if minMs <= 0 {
		minMs = 100
	}
	if maxMs < minMs {
		maxMs = minMs
	}

	delta := maxMs - minMs
	delayMs := minMs
	if delta > 0 {
		delayMs += rand.Intn(delta)
	}

	select {
	case <-time.After(time.Duration(delayMs) * time.Millisecond):
	case <-ctx.Done():
	}
}
