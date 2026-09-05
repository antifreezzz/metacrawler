package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"metacrawler/internal/config"
	"metacrawler/internal/domain"
	"metacrawler/internal/llm"
	"metacrawler/internal/storage"
	"metacrawler/internal/worker"
)

type Server struct {
	db             *storage.DB
	workerMgr      *worker.Manager
	llmClient      *llm.Client
	cfg            *config.Config
	router         *http.ServeMux
	indexTemplate  *template.Template
	detailTemplate *template.Template
	listTemplate   *template.Template
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

	s.indexTemplate = template.Must(template.New("layout.html").Funcs(funcMap).ParseFiles(layout, index, gamesList))
	s.detailTemplate = template.Must(template.New("layout.html").Funcs(funcMap).ParseFiles(layout, detail))
	s.listTemplate = template.Must(template.New("games_list.html").Funcs(funcMap).ParseFiles(gamesList))
}

func (s *Server) routes() {
	s.router.HandleFunc("GET /", s.handleIndex)
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
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.detailTemplate.ExecuteTemplate(w, "layout.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleWorkerRun(w http.ResponseWriter, r *http.Request) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		_, _ = s.workerMgr.ExecuteCycle(ctx)
	}()

	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"started"}`))
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

			html := fmt.Sprintf(`<div id="worker-status-badge" class="flex items-center space-x-2 text-xs px-3 py-1.5 rounded-full bg-slate-800 border border-slate-700"><span class="w-2 h-2 rounded-full %s"></span><span class="text-slate-300 font-medium">%s</span></div>`, badgeColor, template.HTMLEscapeString(taskDesc))

			_, _ = fmt.Fprintf(w, "event: status\ndata: %s\n\n", html)
			flusher.Flush()
		}
	}
}
