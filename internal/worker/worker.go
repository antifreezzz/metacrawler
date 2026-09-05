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
}

type YouTubeClient interface {
	AnalyzeVideo(ctx context.Context, gameID, gameTitle string) (*domain.YouTubeAnalysis, error)
}

type StatusInfo struct {
	Status         string    `json:"status"` // Idle, Running, Error
	CurrentTask    string    `json:"current_task"`
	ProcessedCount int       `json:"processed_count"`
	TotalInBatch   int       `json:"total_in_batch"`
	CurrentIndex   int       `json:"current_index"`
	LastRunAt      time.Time `json:"last_run_at"`
	LastError      string    `json:"last_error"`
	CurrentPage    string    `json:"current_page"`
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
	}
}

func (m *Manager) StartCron() error {
	m.cron = cron.New()
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
	ch := make(chan StatusInfo, 10)
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

	return StatusInfo{
		Status:         m.status,
		CurrentTask:    m.currentTask,
		ProcessedCount: m.processedCount,
		TotalInBatch:   m.totalInBatch,
		CurrentIndex:   m.currentIndex,
		LastRunAt:      m.lastRunAt,
		LastError:      m.lastError,
		CurrentPage:    curPage,
	}
}

func (m *Manager) setRunning(task string, total int) {
	m.mu.Lock()
	m.status = "Running"
	m.currentTask = task
	m.totalInBatch = total
	m.currentIndex = 0
	m.lastError = ""
	m.mu.Unlock()
	m.notifySubscribers()
}

func (m *Manager) setProgress(current int, task string) {
	m.mu.Lock()
	m.currentIndex = current
	m.currentTask = task
	m.mu.Unlock()
	m.notifySubscribers()
}

func (m *Manager) setFinished(processed int, err error) {
	m.mu.Lock()
	m.lastRunAt = time.Now().UTC()
	m.processedCount = processed
	if err != nil {
		m.status = "Error"
		m.lastError = err.Error()
		m.currentTask = fmt.Sprintf("Error: %v", err)
	} else {
		m.status = "Idle"
		m.currentTask = fmt.Sprintf("Finished. Processed %d games.", processed)
	}
	m.mu.Unlock()
	m.notifySubscribers()
}

// ExecuteCycle выполняет один цикл сбора данных: либо New Releases (в начале дня), либо очередную страницу каталога (строго без добора).
func (m *Manager) ExecuteCycle(ctx context.Context) (int, error) {
	m.mu.Lock()
	if m.status == "Running" {
		m.mu.Unlock()
		return 0, fmt.Errorf("worker is already running")
	}
	m.mu.Unlock()

	today := time.Now().UTC().Format("2006-01-02")
	lastCrawlDate, err := m.db.GetState(ctx, "last_crawl_date")
	if err != nil {
		m.setFinished(0, err)
		return 0, err
	}

	var candidateSlugs []string
	isFirstRunToday := lastCrawlDate != today

	if isFirstRunToday {
		m.setRunning("Fetching New Releases", 0)
		candidateSlugs, err = m.scraper.FetchNewReleases(ctx)
		if err != nil {
			m.setFinished(0, fmt.Errorf("fetch new releases: %w", err))
			return 0, err
		}
		// Запоминаем, что сегодня New Releases обработаны, и для последующих запусков страница = 1
		_ = m.db.SetState(ctx, "last_crawl_date", today)
		_ = m.db.SetState(ctx, "current_page", "1")
	} else {
		curPageStr, _ := m.db.GetState(ctx, "current_page")
		curPage := 1
		if p, err := strconv.Atoi(curPageStr); err == nil && p >= 1 {
			curPage = p
		}

		m.setRunning(fmt.Sprintf("Fetching Browse Catalog (Page %d)", curPage), 0)
		candidateSlugs, err = m.scraper.FetchBrowsePage(ctx, curPage)
		if err != nil {
			m.setFinished(0, fmt.Errorf("fetch browse page %d: %w", curPage, err))
			return 0, err
		}
		// Переходим к следующей странице на следующий запуск
		_ = m.db.SetState(ctx, "current_page", strconv.Itoa(curPage+1))
	}

	// Отбираем только игры, которые сегодня ЕЩЕ НЕ обрабатывались (СТРОГО БЕЗ ДОБОРА со следующих страниц)
	var toProcess []string
	for _, slug := range candidateSlugs {
		processedToday, checkErr := m.db.IsProcessedOnDate(ctx, slug, today)
		if checkErr == nil && !processedToday {
			toProcess = append(toProcess, slug)
		}
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

		game, reviews, scrapeErr := m.scraper.FetchGameDetails(ctx, slug)
		if scrapeErr != nil {
			continue // пропускаем битую карточку, продолжаем батч
		}

		// 1. Сохранение игры и её платформ
		if err := m.db.UpsertGame(ctx, game); err != nil {
			continue
		}

		// Получаем сохраненную игру для актуальных platform IDs
		savedGame, err := m.db.GetGameBySlug(ctx, slug)
		if err != nil || savedGame == nil {
			continue
		}

		// 2. Распределение отзывов по платформам и дедубликация
		for _, p := range savedGame.Platforms {
			var platReviews []domain.Review
			for _, r := range reviews {
				r.GamePlatformID = p.ID
				platReviews = append(platReviews, r)
			}
			_, _ = m.db.SaveReviews(ctx, platReviews)

			// 3. Выборка всех отзывов по платформе (включая ранее сохраненные) для саммари
			allPlatReviews, _ := m.db.GetReviewsByPlatformID(ctx, p.ID)
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
				if llmErr == nil && summary != nil {
					_ = m.db.UpsertPlatformSummary(ctx, &domain.PlatformSummary{
						GamePlatformID: p.ID,
						CriticPros:     summary.CriticPros,
						CriticCons:     summary.CriticCons,
						UserPros:       summary.UserPros,
						UserCons:       summary.UserCons,
					})
				}
			}
		}

		// 5. Векторный эмбеддинг игры
		textForEmbedding := fmt.Sprintf("%s. %s. Developer: %s", savedGame.Title, savedGame.Description, savedGame.Developer)
		vec, embErr := m.llm.GetEmbedding(ctx, textForEmbedding)
		if embErr == nil && len(vec) > 0 {
			_ = m.db.SaveEmbedding(ctx, &domain.GameEmbedding{
				GameID:     savedGame.ID,
				Vector:     vec,
				Dimensions: len(vec),
			})
		}

		// 6. Дополнительная часть 1: Анализ популярного летсплея на YouTube
		if m.youtube != nil {
			m.setProgress(idx+1, fmt.Sprintf("Analyzing YouTube letsplay for: %s", savedGame.Title))
			ytAnalysis, ytErr := m.youtube.AnalyzeVideo(ctx, savedGame.ID, savedGame.Title)
			if ytErr == nil && ytAnalysis != nil {
				_ = m.db.UpsertYouTubeAnalysis(ctx, ytAnalysis)
			}
		}

		// 7. Помечаем игру как обработанную сегодня
		_ = m.db.MarkProcessed(ctx, slug, today)
		processedCount++
	}

	m.setFinished(processedCount, nil)
	return processedCount, nil
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
