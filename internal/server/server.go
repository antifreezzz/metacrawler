package server

import (
	"context"
	"encoding/json"
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
	router             *http.ServeMux
	indexTemplate      *template.Template
	detailTemplate     *template.Template
	listTemplate       *template.Template
	monitoringTemplate *template.Template
}

func New(db *storage.DB, workerMgr *worker.Manager, llmClient *llm.Client, cfg *config.Config) *Server {
	s := &Server{
		db:        db,
		workerMgr: workerMgr,
		llmClient: llmClient,
		cfg:       cfg,
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
	}

	layout := filepath.Join(tmplDir, "layout.html")
	index := filepath.Join(tmplDir, "index.html")
	gamesList := filepath.Join(tmplDir, "games_list.html")
	detail := filepath.Join(tmplDir, "game_detail.html")
	monitoring := filepath.Join(tmplDir, "monitoring.html")

	s.indexTemplate = template.Must(template.New("layout.html").Funcs(funcMap).ParseFiles(layout, index, gamesList))
	s.detailTemplate = template.Must(template.New("layout.html").Funcs(funcMap).ParseFiles(layout, detail))
	s.listTemplate = template.Must(template.New("games_list.html").Funcs(funcMap).ParseFiles(gamesList))
	s.monitoringTemplate = template.Must(template.New("layout.html").Funcs(funcMap).ParseFiles(layout, monitoring))
}

func (s *Server) routes() {
	s.router.HandleFunc("GET /", s.handleIndex)
	s.router.HandleFunc("GET /monitoring", s.handleMonitoring)
	s.router.HandleFunc("GET /api/games", s.handleGamesList)
	s.router.HandleFunc("GET /games/{slug}", s.handleGameDetail)
	s.router.HandleFunc("POST /api/worker/run", s.handleWorkerRun)
	s.router.HandleFunc("GET /api/worker/status", s.handleWorkerStatus)
	s.router.HandleFunc("GET /api/worker/events", s.handleWorkerEvents)
}

type IndexPageData struct {
	Filter      storage.ListFilter
	Platforms   []string
	Games       []domain.Game
	CurrentPage string
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	filter := storage.ListFilter{
		Search:   r.URL.Query().Get("search"),
		Platform: r.URL.Query().Get("platform"),
		Sort:     r.URL.Query().Get("sort"),
		Limit:    100,
	}
	if filter.Sort == "" {
		filter.Sort = "newest"
	}

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
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.indexTemplate.ExecuteTemplate(w, "layout.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleGamesList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	filter := storage.ListFilter{
		Search:   r.URL.Query().Get("search"),
		Platform: r.URL.Query().Get("platform"),
		Sort:     r.URL.Query().Get("sort"),
		Limit:    100,
	}
	if filter.Sort == "" {
		filter.Sort = "newest"
	}

	games, err := s.db.ListGames(ctx, filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := IndexPageData{
		Filter: filter,
		Games:  games,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.listTemplate.ExecuteTemplate(w, "games_list", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type DetailPageData struct {
	Game         *domain.Game
	SimilarGames []domain.Game
	YouTube      *domain.YouTubeAnalysis
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

	// Подтягиваем резюме отзывов для каждой платформы
	for i := range game.Platforms {
		summary, _ := s.db.GetPlatformSummary(ctx, game.Platforms[i].ID)
		if summary != nil {
			game.Platforms[i].Summary = summary
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
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.detailTemplate.ExecuteTemplate(w, "layout.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type MonitoringPageData struct {
	Status worker.StatusInfo
}

func (s *Server) handleMonitoring(w http.ResponseWriter, r *http.Request) {
	status := s.workerMgr.GetStatus()
	data := MonitoringPageData{Status: status}
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
			badgeColor := "bg-emerald-400"
			if status.Status == "Running" {
				badgeColor = "bg-amber-400 animate-pulse"
			} else if status.Status == "Error" {
				badgeColor = "bg-rose-500"
			}

			taskDesc := status.CurrentTask
			if taskDesc == "" {
				taskDesc = "Воркер: Готов"
			}

			// 1. Событие для бейджа в шапке (обновляет внутренности #worker-status-badge)
			badgeHTML := fmt.Sprintf(`<span class="w-2 h-2 rounded-full %s"></span><span class="text-slate-300 font-medium">%s</span>`, badgeColor, template.HTMLEscapeString(taskDesc))
			_, _ = fmt.Fprintf(w, "event: status\ndata: %s\n\n", strings.ReplaceAll(badgeHTML, "\n", ""))

			// 2. Событие для панели метрик мониторинга (обновляет внутренности #metrics-panel)
			lastRunStr := "Еще не запускался"
			if !status.LastRunAt.IsZero() {
				lastRunStr = status.LastRunAt.Format("15:04:05 (02.01)")
			}
			metricsHTML := fmt.Sprintf(`<div class="bg-slate-900/60 p-5 rounded-2xl border border-slate-800 space-y-1"><div class="text-xs uppercase font-bold text-slate-400">Статус воркера</div><div class="text-2xl font-black text-white flex items-center space-x-2"><span class="w-3 h-3 rounded-full %s"></span><span>%s</span></div><div class="text-xs text-slate-500 truncate pt-1">%s</div></div><div class="bg-slate-900/60 p-5 rounded-2xl border border-slate-800 space-y-1"><div class="text-xs uppercase font-bold text-slate-400">Обработано в батче</div><div class="text-2xl font-black text-emerald-400">%d / %d</div><div class="text-xs text-slate-500 pt-1">Успешно: %d игр</div></div><div class="bg-slate-900/60 p-5 rounded-2xl border border-slate-800 space-y-1"><div class="text-xs uppercase font-bold text-slate-400">Текущая страница каталога</div><div class="text-2xl font-black text-indigo-400">№ %s</div><div class="text-xs text-slate-500 pt-1">SEE ALL пагинация</div></div><div class="bg-slate-900/60 p-5 rounded-2xl border border-slate-800 space-y-1"><div class="text-xs uppercase font-bold text-slate-400">Последний запуск</div><div class="text-lg font-bold text-slate-300">%s</div><div class="text-xs text-slate-500 pt-1">Расписание: 1 раз в час</div></div>`,
				badgeColor, status.Status, template.HTMLEscapeString(taskDesc),
				status.CurrentIndex, status.TotalInBatch, status.ProcessedCount,
				status.CurrentPage, lastRunStr,
			)
			_, _ = fmt.Fprintf(w, "event: metrics\ndata: %s\n\n", strings.ReplaceAll(metricsHTML, "\n", ""))

			// 3. Событие для логов (обновляет внутренности #worker-logs)
			var logsBuilder strings.Builder
			for _, l := range status.Logs {
				logsBuilder.WriteString(fmt.Sprintf(`<div class="text-slate-400"><span class="text-emerald-400">&gt;</span> %s</div>`, template.HTMLEscapeString(l)))
			}
			_, _ = fmt.Fprintf(w, "event: logs\ndata: %s\n\n", strings.ReplaceAll(logsBuilder.String(), "\n", ""))

			flusher.Flush()
		}
	}
}
