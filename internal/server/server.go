package server

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"metacrawler/internal/config"
	"metacrawler/internal/domain"
	"metacrawler/internal/llm"
	"metacrawler/internal/storage"
	"metacrawler/internal/worker"
)

type Server struct {
	db                 *storage.DB
	workerMgr          *worker.Manager
	llmClient          *llm.Client
	cfg                *config.Config
	auth               *AuthManager
	router             *http.ServeMux
	indexTemplate      *template.Template
	detailTemplate     *template.Template
	listTemplate       *template.Template
	monitoringTemplate *template.Template
	loginTemplate      *template.Template
}

func New(db *storage.DB, workerMgr *worker.Manager, llmClient *llm.Client, cfg *config.Config) *Server {
	s := &Server{
		db:        db,
		workerMgr: workerMgr,
		llmClient: llmClient,
		cfg:       cfg,
		auth:      NewAuthManager(cfg.AdminUsername, cfg.AdminPassword, cfg.SessionSecret),
		router:    http.NewServeMux(),
	}

	s.loadTemplates()
	s.routes()
	return s
}

func (s *Server) Router() *http.ServeMux {
	return s.router
}

func (s *Server) loadTemplates() {
	paths := []string{"web/templates", "../../web/templates"}
	var tmplDir string
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			tmplDir = p
			break
		}
	}
	if tmplDir == "" {
		tmplDir = "web/templates"
	}

	funcMap := template.FuncMap{
		"isScoreHigh": func(score *int) bool {
			return score != nil && *score >= 75
		},
		"isScoreMid": func(score *int) bool {
			return score != nil && *score >= 50 && *score < 75
		},
		"isScoreLow": func(score *int) bool {
			return score != nil && *score < 50
		},
		"formatScore": func(score *int) string {
			if score == nil {
				return "tbd"
			}
			return fmt.Sprintf("%d", *score)
		},
		"formatUserScore": func(score *float64) string {
			if score == nil {
				return "tbd"
			}
			return fmt.Sprintf("%.1f", *score)
		},
		"formatMetascoreDelta": func(cur *int, hist []domain.ScorePoint) string {
			if cur == nil || len(hist) < 2 {
				return ""
			}
			prev := hist[len(hist)-2].Metascore
			if prev == nil {
				return ""
			}
			delta := *cur - *prev
			if delta == 0 {
				return ""
			}
			return fmt.Sprintf("%+d", delta)
		},
		"formatUserscoreDelta": func(cur *float64, hist []domain.ScorePoint) string {
			if cur == nil || len(hist) < 2 {
				return ""
			}
			prev := hist[len(hist)-2].Userscore
			if prev == nil {
				return ""
			}
			delta := *cur - *prev
			if delta == 0 {
				return ""
			}
			return fmt.Sprintf("%+.1f", delta)
		},
		"scoreDeltaDir": func(delta string) string {
			switch {
			case strings.HasPrefix(delta, "+"):
				return "up"
			case strings.HasPrefix(delta, "-"):
				return "down"
			}
			return ""
		},
		"formatReleaseDate": func(dateStr string) string {
			if dateStr == "" {
				return ""
			}
			t, err := time.Parse("2006-01-02", dateStr)
			if err != nil {
				return dateStr
			}
			months := []string{
				"", "янв", "фев", "мар", "апр", "май", "июн",
				"июл", "авг", "сен", "окт", "ноя", "дек",
			}
			m := int(t.Month())
			if m >= 1 && m <= 12 {
				return fmt.Sprintf("%d %s %d", t.Day(), months[m], t.Year())
			}
			return t.Format("02.01.2006")
		},
		"splitParagraphs": func(text string) []string {
			var out []string
			for _, p := range strings.Split(text, "\n") {
				if p = strings.TrimSpace(p); p != "" {
					out = append(out, p)
				}
			}
			return out
		},
		"oneline": func(text string) string {
			return strings.Join(strings.Fields(text), " ")
		},
	}

	layout := filepath.Join(tmplDir, "layout.html")
	index := filepath.Join(tmplDir, "index.html")
	gamesList := filepath.Join(tmplDir, "games_list.html")
	detail := filepath.Join(tmplDir, "game_detail.html")
	monitoring := filepath.Join(tmplDir, "monitoring.html")
	login := filepath.Join(tmplDir, "login.html")

	s.indexTemplate = template.Must(template.New("layout.html").Funcs(funcMap).ParseFiles(layout, index, gamesList))
	s.detailTemplate = template.Must(template.New("layout.html").Funcs(funcMap).ParseFiles(layout, detail))
	s.listTemplate = template.Must(template.New("games_list.html").Funcs(funcMap).ParseFiles(gamesList))
	s.monitoringTemplate = template.Must(template.New("layout.html").Funcs(funcMap).ParseFiles(layout, monitoring))
	s.loginTemplate = template.Must(template.New("layout.html").Funcs(funcMap).ParseFiles(layout, login))
}

func (s *Server) routes() {
	s.router.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})
	s.router.HandleFunc("GET /", s.handleIndex)
	s.router.HandleFunc("GET /login", s.handleLoginPage)
	s.router.HandleFunc("POST /login", s.handleLoginSubmit)
	s.router.HandleFunc("GET /logout", s.handleLogout)
	s.router.HandleFunc("POST /logout", s.handleLogout)
	s.router.HandleFunc("GET /api/games", s.handleGamesList)
	s.router.HandleFunc("GET /games/{slug}", s.handleGameDetail)
	s.router.HandleFunc("POST /api/games/{slug}/recrawl", s.RequireAuth(s.handleGameRecrawl))

	// Защищенные маршруты администрирования и управления сбором
	s.router.HandleFunc("GET /monitoring", s.RequireAuth(s.handleMonitoring))
	s.router.HandleFunc("POST /api/worker/run", s.RequireAuth(s.handleWorkerRun))
	s.router.HandleFunc("GET /api/worker/status", s.RequireAuth(s.handleWorkerStatus))
	s.router.HandleFunc("GET /api/worker/events", s.RequireAuth(s.handleWorkerEvents))

	// Публичный RSS-фид последних добавленных игр
	s.router.HandleFunc("GET /rss", s.handleRSS)
}

type rssChannel struct {
	XMLName     xml.Name  `xml:"rss"`
	Version     string    `xml:"version,attr"`
	Title       string    `xml:"channel>title"`
	Link        string    `xml:"channel>link"`
	Description string    `xml:"channel>description"`
	Language    string    `xml:"channel>language"`
	LastBuild   string    `xml:"channel>lastBuildDate"`
	Items       []rssItem `xml:"channel>item"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	PubDate     string `xml:"pubDate"`
	Description string `xml:"description"`
}

func (s *Server) handleRSS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	games, err := s.db.ListGames(ctx, storage.ListFilter{Sort: "newest", Limit: 30})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	items := make([]rssItem, 0, len(games))
	for _, g := range games {
		link := fmt.Sprintf("/games/%s", g.Slug)
		desc := g.Description
		if desc == "" {
			desc = "Описание пока не собрано."
		}
		pubDate := g.CreatedAt.UTC().Format(time.RFC1123Z)
		items = append(items, rssItem{
			Title:       g.Title,
			Link:        link,
			GUID:        link,
			PubDate:     pubDate,
			Description: desc,
		})
	}

	feed := rssChannel{
		XMLName:     xml.Name{Local: "rss"},
		Version:     "2.0",
		Title:       "Metacrawler — новые игры",
		Link:        "/",
		Description: "Последние добавленные игры из каталога Metacritic с оценками и ИИ-анализом отзывов.",
		Language:    "ru",
		LastBuild:   time.Now().UTC().Format(time.RFC1123Z),
		Items:       items,
	}

	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(feed); err != nil {
		return
	}
	_, _ = w.Write([]byte("\n"))
}

type PaginationInfo struct {
	CurrentPage int
	TotalPages  int
	TotalItems  int
	PageSize    int
	HasPrev     bool
	HasNext     bool
	PrevPage    int
	NextPage    int
	Pages       []int
	StartItem   int
	EndItem     int
}

type IndexPageData struct {
	Filter      storage.ListFilter
	Platforms   []string
	Games       []domain.Game
	CurrentPage string
	Pagination  PaginationInfo
	IsAdmin     bool
}

func buildPagination(currentPage, totalItems, pageSize int) PaginationInfo {
	if pageSize <= 0 {
		pageSize = 24
	}
	totalPages := (totalItems + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}
	if currentPage < 1 {
		currentPage = 1
	} else if currentPage > totalPages {
		currentPage = totalPages
	}

	startItem := (currentPage-1)*pageSize + 1
	endItem := currentPage * pageSize
	if endItem > totalItems {
		endItem = totalItems
	}
	if totalItems == 0 {
		startItem = 0
		endItem = 0
	}

	var pages []int
	for i := 1; i <= totalPages; i++ {
		pages = append(pages, i)
	}

	return PaginationInfo{
		CurrentPage: currentPage,
		TotalPages:  totalPages,
		TotalItems:  totalItems,
		PageSize:    pageSize,
		HasPrev:     currentPage > 1,
		HasNext:     currentPage < totalPages,
		PrevPage:    currentPage - 1,
		NextPage:    currentPage + 1,
		Pages:       pages,
		StartItem:   startItem,
		EndItem:     endItem,
	}
}

func (s *Server) isAuthenticated(r *http.Request) bool {
	return s.auth.ValidateRequest(r)
}

type LoginPageData struct {
	Username string
	Next     string
	Error    string
	IsAdmin  bool
}

func sanitizeRedirectURL(target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return "/monitoring"
	}
	// Защита от Open Redirect (CWE-601): разрешаем только относительные пути внутри сайта.
	// Запрещаем protocol-relative (//) и обратные слэши (/\)
	if strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "//") && !strings.HasPrefix(target, "/\\") {
		return target
	}
	return "/monitoring"
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.isAuthenticated(r) {
		http.Redirect(w, r, "/monitoring", http.StatusFound)
		return
	}
	next := sanitizeRedirectURL(r.URL.Query().Get("next"))
	data := LoginPageData{
		Next: next,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.loginTemplate.ExecuteTemplate(w, "layout.html", data)
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	username := r.FormValue("username")
	password := r.FormValue("password")
	next := sanitizeRedirectURL(r.FormValue("next"))

	if !s.auth.Authenticate(username, password) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		data := LoginPageData{
			Username: username,
			Next:     next,
			Error:    "Неверное имя пользователя или пароль",
		}
		_ = s.loginTemplate.ExecuteTemplate(w, "layout.html", data)
		return
	}

	cookie := s.auth.GenerateSessionCookie(username)
	http.SetCookie(w, cookie)
	http.Redirect(w, r, next, http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.auth.ClearSessionCookie())
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	page := 1
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 0 {
		page = p
	}
	const pageSize = 24

	filter := storage.ListFilter{
		Search:   r.URL.Query().Get("search"),
		Platform: r.URL.Query().Get("platform"),
		Sort:     r.URL.Query().Get("sort"),
		Limit:    pageSize,
		Offset:   (page - 1) * pageSize,
	}
	if filter.Sort == "" {
		filter.Sort = "newest"
	}

	totalItems, err := s.db.CountGames(ctx, filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	pagination := buildPagination(page, totalItems, pageSize)
	filter.Offset = (pagination.CurrentPage - 1) * pageSize

	games, err := s.db.ListGames(ctx, filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	platforms, _ := s.db.GetDistinctPlatforms(ctx)
	curPage, _ := s.db.GetState(ctx, "current_page")
	if curPage == "" {
		curPage = "1"
	}

	data := IndexPageData{
		Filter:      filter,
		Platforms:   platforms,
		Games:       games,
		CurrentPage: curPage,
		Pagination:  pagination,
		IsAdmin:     s.isAuthenticated(r),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.indexTemplate.ExecuteTemplate(w, "layout.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleGamesList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	page := 1
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 0 {
		page = p
	}
	const pageSize = 24

	filter := storage.ListFilter{
		Search:   r.URL.Query().Get("search"),
		Platform: r.URL.Query().Get("platform"),
		Sort:     r.URL.Query().Get("sort"),
		Limit:    pageSize,
		Offset:   (page - 1) * pageSize,
	}
	if filter.Sort == "" {
		filter.Sort = "newest"
	}

	totalItems, err := s.db.CountGames(ctx, filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	pagination := buildPagination(page, totalItems, pageSize)
	filter.Offset = (pagination.CurrentPage - 1) * pageSize

	games, err := s.db.ListGames(ctx, filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := IndexPageData{
		Filter:     filter,
		Games:      games,
		Pagination: pagination,
		IsAdmin:    s.isAuthenticated(r),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Replace-Url", "/?"+r.URL.Query().Encode())
	}
	if err := s.listTemplate.ExecuteTemplate(w, "games_list", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleGameRecrawl(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) >= 3 {
			slug = parts[2]
		}
	}
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}

	started, err := s.workerMgr.RecrawlGameAsync(slug)
	if err != nil {
		http.Error(w, fmt.Sprintf("recrawl error: %v", err), http.StatusInternalServerError)
		return
	}

	if !started {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"status":"already_running","slug":"` + slug + `","message":"Пересбор данных для этой игры уже выполняется в фоне"}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"accepted","slug":"` + slug + `","message":"Пересбор запущен в фоне"}`))
}

type DetailPageData struct {
	Game         *domain.Game
	SimilarGames []domain.Game
	YouTube      *domain.YouTubeAnalysis
	IsAdmin      bool
	OGBaseURL    string
}

// schemeFromRequest определяет схему публичного URL (с учетом обратного прокси).
func schemeFromRequest(r *http.Request) string {
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		return p
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func (s *Server) handleGameDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	if slug == "" {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) >= 2 {
			slug = parts[1]
		}
	}

	game, err := s.db.GetGameBySlug(ctx, slug)
	if err != nil || game == nil {
		http.NotFound(w, r)
		return
	}

	// Подтягиваем резюме и историю оценок для каждой платформы
	for i := range game.Platforms {
		summary, _ := s.db.GetPlatformSummary(ctx, game.Platforms[i].ID)
		reviews, _ := s.db.GetReviewsByPlatformID(ctx, game.Platforms[i].ID)
		game.Platforms[i].Reviews = reviews
		game.Platforms[i].ScoreHistory, _ = s.db.GetScoreHistory(ctx, game.Platforms[i].ID)
		if summary != nil {
			game.Platforms[i].Summary = summary
		} else if len(reviews) > 0 {
			// На лету генерируем резюме отзывов (с фоллбэком)
			var critics, users []domain.Review
			for _, r := range reviews {
				if r.ReviewType == domain.ReviewTypeCritic {
					critics = append(critics, r)
				} else {
					users = append(users, r)
				}
			}
			sumRes, llmErr := s.llmClient.SummarizeReviews(ctx, game.Title, game.Platforms[i].Platform, critics, users)
			if llmErr == nil && sumRes != nil {
				newSum := &domain.PlatformSummary{
					GamePlatformID: game.Platforms[i].ID,
					CriticPros:     sumRes.CriticPros,
					CriticCons:     sumRes.CriticCons,
					UserPros:       sumRes.UserPros,
					UserCons:       sumRes.UserCons,
				}
				_ = s.db.UpsertPlatformSummary(ctx, newSum)
				game.Platforms[i].Summary = newSum
			}
		}
	}

	// Подтягиваем YouTube анализ летсплея
	ytAnalysis, _ := s.db.GetYouTubeAnalysis(ctx, game.ID)

	// Подбор похожих игр на основе эмбеддингов
	var similarGames []domain.Game
	allEmbeddings, _ := s.db.GetAllEmbeddings(ctx)
	var targetVec []float32
	for _, emb := range allEmbeddings {
		if emb.GameID == game.ID {
			targetVec = emb.Vector
			break
		}
	}

	if len(targetVec) > 0 {
		topSimilar := llm.FindTopSimilar(game.ID, targetVec, allEmbeddings, 5)
		for _, sim := range topSimilar {
			simGame, err := s.db.GetGameByID(ctx, sim.GameID)
			if err == nil && simGame != nil {
				similarGames = append(similarGames, *simGame)
			}
		}
	}

	data := DetailPageData{
		Game:         game,
		SimilarGames: similarGames,
		YouTube:      ytAnalysis,
		IsAdmin:      s.isAuthenticated(r),
		OGBaseURL:    schemeFromRequest(r) + "://" + r.Host,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.detailTemplate.ExecuteTemplate(w, "layout.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type MonitoringPageData struct {
	Status  worker.StatusInfo
	IsAdmin bool
}

func (s *Server) handleMonitoring(w http.ResponseWriter, r *http.Request) {
	status := s.workerMgr.GetStatus()
	data := MonitoringPageData{
		Status:  status,
		IsAdmin: true,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.monitoringTemplate.ExecuteTemplate(w, "layout.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleWorkerRun(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	modeParam := r.URL.Query().Get("mode")
	pageParam := r.URL.Query().Get("page")
	if pageParam == "" {
		pageParam = r.FormValue("page")
	}

	runMode := worker.RunModeAuto
	switch modeParam {
	case "new_releases":
		runMode = worker.RunModeNewReleases
	case "next_page":
		runMode = worker.RunModeNextPage
	case "custom_page":
		runMode = worker.RunModeCustomPage
	}

	pageInt := 0
	if p, err := strconv.Atoi(pageParam); err == nil && p > 0 {
		pageInt = p
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		_, _ = s.workerMgr.ExecuteMode(ctx, runMode, pageInt)
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"accepted","mode":"` + string(runMode) + `"}`))
}

func (s *Server) handleWorkerStatus(w http.ResponseWriter, r *http.Request) {
	status := s.workerMgr.GetStatus()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (s *Server) handleWorkerEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	statusCh, unsubscribe := s.workerMgr.Subscribe()
	defer unsubscribe()

	for {
		select {
		case <-r.Context().Done():
			return
		case status, ok := <-statusCh:
			if !ok {
				return
			}
			badgeColor := "status-ready"
			if status.Status == "Running" {
				badgeColor = "status-running"
			} else if status.Status == "Error" {
				badgeColor = "status-error"
			}

			taskDesc := status.CurrentTask
			if taskDesc == "" {
				taskDesc = "Воркер: Готов"
			}

			// 1. Событие для бейджа в шапке (обновляет внутренности #worker-status-badge)
			badgeHTML := fmt.Sprintf(`<span class="dot %s"></span><span>%s</span>`, badgeColor, template.HTMLEscapeString(taskDesc))
			_, _ = fmt.Fprintf(w, "event: status\ndata: %s\n\n", strings.ReplaceAll(badgeHTML, "\n", ""))

			// 2. Событие для панели метрик мониторинга (обновляет внутренности #metrics-panel)
			lastRunStr := "Еще не запускался"
			if !status.LastRunAt.IsZero() {
				lastRunStr = status.LastRunAt.In(domain.TimezoneUTC3).Format("15:04:05 (02.01)")
			}
			metricsHTML := fmt.Sprintf(`<div><div>Статус воркера</div><div class="metric-status"><span class="dot %s"></span><span>%s</span></div><div>%s</div></div><div><div>Обработано в партии</div><div>%d / %d</div><div>Успешно: %d игр</div></div><div><div>Страница каталога</div><div>№ %s</div><div>Последовательный обход</div></div><div><div>Последний запуск</div><div>%s</div><div>Автоматически раз в час</div></div>`,
				badgeColor, status.Status, template.HTMLEscapeString(taskDesc),
				status.CurrentIndex, status.TotalInBatch, status.ProcessedCount,
				status.CurrentPage, lastRunStr,
			)
			_, _ = fmt.Fprintf(w, "event: metrics\ndata: %s\n\n", strings.ReplaceAll(metricsHTML, "\n", ""))

			// 3. Событие для логов (обновляет внутренности #worker-logs)
			var logsBuilder strings.Builder
			for _, l := range status.Logs {
				logsBuilder.WriteString(fmt.Sprintf(`<div class="muted">&gt; %s</div>`, template.HTMLEscapeString(l)))
			}
			_, _ = fmt.Fprintf(w, "event: logs\ndata: %s\n\n", strings.ReplaceAll(logsBuilder.String(), "\n", ""))

			flusher.Flush()
		}
	}
}
