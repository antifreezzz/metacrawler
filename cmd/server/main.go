package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"metacrawler/internal/config"
	"metacrawler/internal/llm"
	"metacrawler/internal/scraper"
	"metacrawler/internal/server"
	"metacrawler/internal/storage"
	"metacrawler/internal/worker"
	"metacrawler/internal/youtube"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg := config.Load()

	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}

	// Гарантируем наличие директории для БД SQLite
	if err := os.MkdirAll("data", 0755); err != nil {
		log.Fatalf("failed to create data dir: %v", err)
	}

	db, err := storage.New(cfg.DBPath)
	if err != nil {
		log.Fatalf("failed to initialize sqlite db: %v", err)
	}
	defer db.Close()

	scraperClient, err := scraper.NewClient()
	if err != nil {
		log.Fatalf("failed to initialize scraper client: %v", err)
	}

	llmClient := llm.NewClientWithEmbedding(
		cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel,
		cfg.EmbeddingEngine, cfg.EmbeddingBaseURL, cfg.EmbeddingAPIKey, cfg.EmbeddingModel,
	)
	if cfg.LLMTimeoutSeconds > 0 {
		llmClient.SetTimeout(time.Duration(cfg.LLMTimeoutSeconds) * time.Second)
	}
	llmClient.SetTranscriptMaxChars(cfg.TranscriptMaxChars)

	ytClient := youtube.NewClientWithWhisperURL(llmClient, cfg.WhisperURL, cfg.WhisperBinaryPath, cfg.WhisperModelPath, cfg.YouTubeCookiesPath, cfg.WhisperWindowSeconds, cfg.WhisperMaxWindows)

	workerMgr := worker.NewManager(db, scraperClient, llmClient, ytClient, cfg)

	// Запуск фонового планировщика (1 раз в час)
	if err := workerMgr.StartCron(); err != nil {
		log.Printf("Warning: failed to start cron scheduler: %v", err)
	} else {
		log.Printf("Cron scheduler started with schedule: %s", cfg.CronSchedule)
	}
	defer workerMgr.StopCron()

	// Фоновая проверка и догенерация недостающих резюме для ранее собранных игр
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		if count, err := workerMgr.BackfillMissingSummaries(ctx); err == nil && count > 0 {
			log.Printf("Backfill completed: generated %d missing review summaries", count)
		}
	}()

	srv := server.New(db, workerMgr, llmClient, cfg)

	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Router(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh

		log.Println("Shutting down server gracefully...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()

	fmt.Printf("\n=======================================================\n")
	fmt.Printf("🚀 METACRAWLER SERVICE STARTED ON http://localhost:%s\n", cfg.Port)
	fmt.Printf("📦 Database: %s\n", cfg.DBPath)
	effectiveModel := llmClient.GetOrDiscoverModel(context.Background())
	fmt.Printf("🤖 LLM Model: %s (Endpoint: %s)\n", effectiveModel, cfg.LLMBaseURL)
	fmt.Printf("=======================================================\n\n")

	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http server error: %v", err)
	}
}
