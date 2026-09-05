package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"metacrawler/internal/config"
	"metacrawler/internal/llm"
	"metacrawler/internal/scraper"
	"metacrawler/internal/storage"
	"metacrawler/internal/worker"
	"metacrawler/internal/server"
	"metacrawler/internal/youtube"
)

func main() {
	cfg := config.Load()

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

	llmClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel, cfg.EmbeddingModel)

	ytClient := youtube.NewClient(llmClient)

	workerMgr := worker.NewManager(db, scraperClient, llmClient, ytClient, cfg)

	// Запуск фонового планировщика (1 раз в час)
	if err := workerMgr.StartCron(); err != nil {
		log.Printf("Warning: failed to start cron scheduler: %v", err)
	} else {
		log.Printf("Cron scheduler started with schedule: %s", cfg.CronSchedule)
	}
	defer workerMgr.StopCron()

	srv := server.New(db, workerMgr, llmClient, cfg)

	httpServer := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: srv.Router(),
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
	fmt.Printf("⏰ Cron Schedule: %s\n", cfg.CronSchedule)
	fmt.Printf("🤖 LLM Model: %s\n", cfg.LLMModel)
	fmt.Printf("=======================================================\n\n")

	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http server error: %v", err)
	}
}
