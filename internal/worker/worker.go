package worker

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
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
	TranslateToRussian(ctx context.Context, text string) (string, error)
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

// ErrGameBusy возвращается, когда игра уже обрабатывается другим циклом или пересбором.
var ErrGameBusy = errors.New("game is already being processed")

// RecrawlOptions определяет, какие части данных игры нужно пересобрать.
type RecrawlOptions struct {
	Scrape      bool // карточка игры и отзывы с Metacritic
	Summaries   bool // LLM-резюме отзывов по платформам
	YouTube     bool // поиск летсплея, транскрипт и саммари
	Embedding   bool // вектор для подбора похожих игр
	Translation bool // русский перевод описания игры
}

// AllRecrawlOptions возвращает пересбор всех частей.
func AllRecrawlOptions() RecrawlOptions {
	return RecrawlOptions{Scrape: true, Summaries: true, YouTube: true, Embedding: true, Translation: true}
}

// Empty сообщает, что ни одна часть не выбрана.
func (o RecrawlOptions) Empty() bool {
	return !o.Scrape && !o.Summaries && !o.YouTube && !o.Embedding && !o.Translation
}

// ParseRecrawlOptions разбирает список частей ("scrape,summaries,youtube,embedding").
// Пустая строка или "all" включает все части. Неизвестные значения игнорируются.
func ParseRecrawlOptions(raw string) RecrawlOptions {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" || raw == "all" {
		return AllRecrawlOptions()
	}
	var opts RecrawlOptions
	for _, part := range strings.Split(raw, ",") {
		switch strings.TrimSpace(part) {
		case "scrape":
			opts.Scrape = true
		case "summaries", "summary":
			opts.Summaries = true
		case "youtube":
			opts.YouTube = true
		case "embedding", "embed":
			opts.Embedding = true
		case "translation", "translate", "trans":
			opts.Translation = true
		case "all":
			return AllRecrawlOptions()
		}
	}
	return opts
}

// String возвращает каноничный список частей для логов и API.
func (o RecrawlOptions) String() string {
	var parts []string
	if o.Scrape {
		parts = append(parts, "scrape")
	}
	if o.Summaries {
		parts = append(parts, "summaries")
	}
	if o.YouTube {
		parts = append(parts, "youtube")
	}
	if o.Embedding {
		parts = append(parts, "embedding")
	}
	if o.Translation {
		parts = append(parts, "translation")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

// RecrawlState описывает состояние фонового пересбора конкретной игры.
type RecrawlState struct {
	Slug       string    `json:"slug"`
	Status     string    `json:"status"` // running, done, error
	Options    string    `json:"options"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Error      string    `json:"error,omitempty"`
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

	activeGames   map[string]struct{}
	recrawlStates map[string]RecrawlState
	activeMu      sync.Mutex
}

func NewManager(db *storage.DB, scraper ScraperClient, llmClient LLMClient, ytClient YouTubeClient, cfg *config.Config) *Manager {
	return &Manager{
		db:            db,
		scraper:       scraper,
		llm:           llmClient,
		youtube:       ytClient,
		cfg:           cfg,
		status:        "Idle",
		subscribers:   make(map[chan StatusInfo]struct{}),
		activeGames:   make(map[string]struct{}),
		recrawlStates: make(map[string]RecrawlState),
		logs:          []string{"[Система] Воркер инициализирован и готов к работе."},
	}
}

func (m *Manager) addLogLocked(msg string) {
	timeStr := domain.Now().Format("15:04:05")
	entry := fmt.Sprintf("[%s] %s", timeStr, msg)
	m.logs = append(m.logs, entry)
	if len(m.logs) > 300 {
		m.logs = m.logs[len(m.logs)-300:]
	}
}

func (m *Manager) addLog(msg string) {
	m.mu.Lock()
	m.addLogLocked(msg)
	m.mu.Unlock()
	m.notifySubscribers()
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
	m.addLogLocked(task)
	m.mu.Unlock()
	m.notifySubscribers()
}

func (m *Manager) setProgress(current int, task string) {
	m.mu.Lock()
	m.currentIndex = current
	m.currentTask = task
	m.addLogLocked(task)
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
		m.addLogLocked(fmt.Sprintf("❌ Ошибка выполнения: %v", err))
	} else {
		m.status = "Idle"
		m.currentTask = fmt.Sprintf("Finished. Processed %d games.", processed)
		m.addLogLocked(fmt.Sprintf("🏁 Цикл завершён (UTC+3). Успешно обработано игр: %d.", processed))
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

		if !m.beginGame(slug) {
			m.addLog(fmt.Sprintf("  ⏭️ [Worker] Игра %s уже обрабатывается в фоне, пропуск", slug))
			continue
		}
		_, err := m.processGame(ctx, slug, today, AllRecrawlOptions(), false)
		m.endGame(slug)
		if err != nil {
			m.addLog(fmt.Sprintf("  ⚠️ [Worker] Ошибка обработки игры %s: %v (пропуск)", slug, err))
			continue
		}
		processedCount++
	}

	m.setFinished(processedCount, nil)
	return processedCount, nil
}

// RecrawlGame выполняет принудительный пересбор выбранных частей данных для одной игры.
func (m *Manager) RecrawlGame(ctx context.Context, slug string, opts RecrawlOptions) (*domain.Game, error) {
	today := domain.Now().Format("2006-01-02")
	if !m.beginGame(slug) {
		return nil, fmt.Errorf("%w: %s", ErrGameBusy, slug)
	}
	defer m.endGame(slug)

	m.addLog(fmt.Sprintf("🔄 [Recrawl] Запущен принудительный пересбор (%s) для игры: %s", opts.String(), slug))
	return m.processGame(ctx, slug, today, opts, true)
}

// RecrawlGameAsync запускает принудительный пересбор данных игры в фоновой горутине.
// Возвращает (false, nil), если пересбор для этой игры уже выполняется в данный момент.
func (m *Manager) RecrawlGameAsync(slug string, opts RecrawlOptions) (bool, error) {
	if opts.Empty() {
		return false, fmt.Errorf("no recrawl parts selected")
	}
	if !m.beginGame(slug) {
		return false, nil
	}

	m.setRecrawlState(RecrawlState{
		Slug:      slug,
		Status:    "running",
		Options:   opts.String(),
		StartedAt: domain.Now(),
	})

	go func() {
		defer m.endGame(slug)

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()

		today := domain.Now().Format("2006-01-02")
		_, err := m.processGame(ctx, slug, today, opts, true)

		state, _ := m.RecrawlStatus(slug)
		state.FinishedAt = domain.Now()
		if err != nil {
			state.Status = "error"
			state.Error = err.Error()
			m.addLog(fmt.Sprintf("❌ [Recrawl] Ошибка пересбора \"%s\": %v", slug, err))
		} else {
			state.Status = "done"
			m.addLog(fmt.Sprintf("🏁 [Recrawl] Пересбор для \"%s\" успешно завершён", slug))
		}
		m.setRecrawlState(state)
	}()

	return true, nil
}

// IsRecrawling проверяет, выполняется ли сейчас обработка для указанного slug.
func (m *Manager) IsRecrawling(slug string) bool {
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	_, active := m.activeGames[slug]
	return active
}

// beginGame резервирует слаг, возвращая false, если он уже обрабатывается.
func (m *Manager) beginGame(slug string) bool {
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	if _, busy := m.activeGames[slug]; busy {
		return false
	}
	m.activeGames[slug] = struct{}{}
	return true
}

func (m *Manager) endGame(slug string) {
	m.activeMu.Lock()
	delete(m.activeGames, slug)
	m.activeMu.Unlock()
}

func (m *Manager) setRecrawlState(state RecrawlState) {
	m.activeMu.Lock()
	m.recrawlStates[state.Slug] = state
	m.activeMu.Unlock()
}

// RecrawlStatus возвращает последнее известное состояние пересбора игры.
func (m *Manager) RecrawlStatus(slug string) (RecrawlState, bool) {
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	state, ok := m.recrawlStates[slug]
	return state, ok
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

func (m *Manager) processGame(ctx context.Context, slug, today string, opts RecrawlOptions, force bool) (*domain.Game, error) {
	var game *domain.Game
	var reviews []domain.Review

	if opts.Scrape {
		var scrapeErr error
		game, reviews, scrapeErr = m.scraper.FetchGameDetails(ctx, slug)
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
	}

	// Получаем сохраненную игру для актуальных platform IDs
	savedGame, err := m.db.GetGameBySlug(ctx, slug)
	if err != nil {
		return nil, fmt.Errorf("get saved game: %w", err)
	}
	if savedGame == nil {
		return nil, fmt.Errorf("game not found: %s", slug)
	}

	// 2. Фиксация истории оценок платформ (пишется только при изменении)
	if opts.Scrape {
		for i := range savedGame.Platforms {
			p := savedGame.Platforms[i]
			if err := m.db.RecordScorePoint(ctx, p.ID, p.Metascore, p.Userscore); err != nil {
				m.addLog(fmt.Sprintf("  ⚠️ [History] Ошибка записи истории оценок (%s): %v", p.Platform, err))
			}
		}
	}

	// 3. Распределение отзывов по платформам: отзыв сохраняется строго в свою
	// платформу. Отзыв без установленной платформы не сохраняется вовсе, чтобы
	// не создавать ложную платформенную привязку.
	type summaryJob struct {
		platformIdx   int
		title         string
		criticReviews []domain.Review
		userReviews   []domain.Review
	}
	var summaryJobs []summaryJob

	savedCounts := make(map[int]int)
	if opts.Scrape {
		unattributed := 0
		for _, r := range reviews {
			if r.Platform == "" {
				unattributed++
			}
		}
		if unattributed > 0 {
			m.addLog(fmt.Sprintf("  ⚠️ [Reviews] Пропущено отзывов без платформы: %d", unattributed))
		}

		for i, p := range savedGame.Platforms {
			var platReviews []domain.Review
			for _, r := range reviews {
				if r.Platform != p.Platform {
					continue
				}
				rr := r
				rr.GamePlatformID = p.ID
				platReviews = append(platReviews, rr)
			}
			c, _ := m.db.SaveReviews(ctx, platReviews)
			savedCounts[i] = c
			if c > 0 {
				m.addLog(fmt.Sprintf("  💾 [Reviews] Платформа %s: новых отзывов %d", p.Platform, c))
			}
		}
	}

	// Гидратация отзывов и резюме для возвращаемого объекта и генерации резюме
	for i := range savedGame.Platforms {
		savedGame.Platforms[i].Reviews, _ = m.db.GetReviewsByPlatformID(ctx, savedGame.Platforms[i].ID)
		if sum, _ := m.db.GetPlatformSummary(ctx, savedGame.Platforms[i].ID); sum != nil {
			savedGame.Platforms[i].Summary = sum
		}
	}

	// 4. Генерация резюме по платформам (только по явному запросу)
	if opts.Summaries {
		for i, p := range savedGame.Platforms {
			// Пропускаем пересбор резюме, если новых отзывов нет и резюме уже есть.
			// Принудительный пересбор (force) игнорирует это условие.
			if !force && savedCounts[i] == 0 && p.Summary != nil {
				continue
			}

			var critics, users []domain.Review
			for _, r := range p.Reviews {
				if r.ReviewType == domain.ReviewTypeCritic {
					critics = append(critics, r)
				} else {
					users = append(users, r)
				}
			}

			if len(critics) > 0 || len(users) > 0 {
				summaryJobs = append(summaryJobs, summaryJob{
					platformIdx:   i,
					title:         savedGame.Title,
					criticReviews: critics,
					userReviews:   users,
				})
			} else {
				m.addLog(fmt.Sprintf("  ℹ️ [Reviews] Платформа %s: отзывов нет", p.Platform))
			}
		}
	}

	// 5. Параллельная генерация резюме по платформам (не зависит от Metacritic, ограничение 3).
	if len(summaryJobs) > 0 {
		type summaryResult struct {
			result *llm.SummaryResult
			err    error
		}
		results := make([]summaryResult, len(savedGame.Platforms))
		sem := make(chan struct{}, 3)
		var wg sync.WaitGroup

		for _, j := range summaryJobs {
			wg.Add(1)
			jj := j
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()

				p := savedGame.Platforms[jj.platformIdx]
				sum, llmErr := m.llm.SummarizeReviews(ctx, jj.title, p.Platform, jj.criticReviews, jj.userReviews)
				if llmErr != nil {
					results[jj.platformIdx] = summaryResult{err: llmErr}
					return
				}
				results[jj.platformIdx] = summaryResult{result: sum}
			}()
		}
		wg.Wait()

		// Обработка и последовательная запись результатов (SQLite - одно соединение)
		for _, j := range summaryJobs {
			res := results[j.platformIdx]
			p := savedGame.Platforms[j.platformIdx]
			if res.err != nil {
				m.addLog(fmt.Sprintf("  ⚠️ [LLM] Ошибка генерации резюме (%s): %v", p.Platform, res.err))
				continue
			}
			if res.result != nil {
				platformSummary := &domain.PlatformSummary{
					GamePlatformID: p.ID,
					CriticPros:     res.result.CriticPros,
					CriticCons:     res.result.CriticCons,
					UserPros:       res.result.UserPros,
					UserCons:       res.result.UserCons,
				}
				_ = m.db.UpsertPlatformSummary(ctx, platformSummary)
				savedGame.Platforms[j.platformIdx].Summary = platformSummary
				if m.llm.HasAPIKey() {
					m.addLog(fmt.Sprintf("  🤖 [LLM] Резюме отзывов (%s) сгенерировано через %s", p.Platform, m.llm.ChatModel()))
				} else {
					m.addLog(fmt.Sprintf("  💡 [LLM] Резюме отзывов (%s) сформировано (локальный фоллбэк, LLM_API_KEY не задан)", p.Platform))
				}
			}
		}
	}

	// 6. Русский перевод описания (кэшируется в games.description_ru).
	// В обычном цикле выполняется только если перевода ещё нет; force перезаписывает.
	if opts.Translation && strings.TrimSpace(savedGame.Description) != "" {
		if force || strings.TrimSpace(savedGame.DescriptionRU) == "" {
			translated, trErr := m.llm.TranslateToRussian(ctx, savedGame.Description)
			if trErr != nil {
				m.addLog(fmt.Sprintf("  ⚠️ [LLM] Ошибка перевода описания: %v", trErr))
			} else if strings.TrimSpace(translated) != "" {
				if err := m.db.SaveGameTranslation(ctx, savedGame.ID, translated); err != nil {
					m.addLog(fmt.Sprintf("  ⚠️ [DB] Ошибка сохранения перевода описания: %v", err))
				} else {
					savedGame.DescriptionRU = translated
					m.addLog("  🌐 [LLM] Описание игры переведено на русский")
				}
			}
		}
	}

	// 7. Эмбеддинг и YouTube-анализ не зависят друг от друга - выполняем параллельно
	var analysisWG sync.WaitGroup

	if opts.Embedding {
		analysisWG.Add(1)
		go func() {
			defer analysisWG.Done()
			m.runEmbedding(ctx, savedGame)
		}()
	}

	if opts.YouTube && m.youtube != nil {
		analysisWG.Add(1)
		go func() {
			defer analysisWG.Done()
			m.runYouTubeAnalysis(ctx, savedGame, force)
		}()
	}

	analysisWG.Wait()

	// 8. Помечаем игру как обработанную (только при полном сборе)
	if opts.Scrape {
		if today != "" {
			_ = m.db.MarkProcessed(ctx, slug, today)
			m.addLog(fmt.Sprintf("  💾 [DB] Игра \"%s\" успешно сохранена в базу на дату %s (UTC+3)", savedGame.Title, today))
		} else {
			m.addLog(fmt.Sprintf("  💾 [DB] Игра \"%s\" успешно сохранена в базу", savedGame.Title))
		}
	}

	return savedGame, nil
}

// runEmbedding генерирует и сохраняет векторное представление игры.
func (m *Manager) runEmbedding(ctx context.Context, savedGame *domain.Game) {
	textForEmbedding := fmt.Sprintf("%s. %s. Developer: %s", savedGame.Title, savedGame.Description, savedGame.Developer)
	vec, embErr := m.llm.GetEmbedding(ctx, textForEmbedding)
	if embErr != nil {
		m.addLog(fmt.Sprintf("  ⚠️ [Embedding] Ошибка получения вектора: %v", embErr))
		return
	}
	if len(vec) == 0 {
		return
	}
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

// runYouTubeAnalysis ищет летсплей и сохраняет саммари. В обычном цикле пропускает
// игру, если анализ уже есть; force=true (пересбор) обновляет его.
func (m *Manager) runYouTubeAnalysis(ctx context.Context, savedGame *domain.Game, force bool) {
	if !force {
		if existing, _ := m.db.GetYouTubeAnalysis(ctx, savedGame.ID); existing != nil {
			return
		}
	}
	ytAnalysis, ytErr := m.youtube.AnalyzeVideo(ctx, savedGame.ID, savedGame.Title)
	if ytErr != nil {
		m.addLog(fmt.Sprintf("  ⚠️ [YouTube] Летсплей не найден или ошибка: %v", ytErr))
	} else if ytAnalysis != nil {
		_ = m.db.UpsertYouTubeAnalysis(ctx, ytAnalysis)
	}
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
