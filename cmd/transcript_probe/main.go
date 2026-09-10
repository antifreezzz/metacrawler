package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"metacrawler/internal/config"
	"metacrawler/internal/llm"
	"metacrawler/internal/youtube"
)

func main() {
	videoID := flag.String("video", "", "YouTube video ID (обязательно)")
	game := flag.String("game", "", "название игры для промпта")
	channel := flag.String("channel", "", "название канала для промпта")
	title := flag.String("title", "", "заголовок видео для промпта")
	durationSec := flag.Int("duration", 0, "длительность видео в секундах (для выбора окон Whisper)")
	strategiesFlag := flag.String("strategies", "full", "список стратегий через запятую")
	budget := flag.Int("budget", 8000, "бюджет символов на фрагмент (для full игнорируется)")
	maxChars := flag.Int("maxchars", -1, "лимит символов перед LLM: -1 = из конфига, 0 = без лимита, >0 = явный")
	dump := flag.String("dump", "", "путь для сохранения полного транскрипта")
	flag.Parse()

	if *videoID == "" {
		log.Fatal("-video обязателен")
	}

	cfg := config.Load()

	llmClient := llm.NewClientWithEmbedding(
		cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel,
		cfg.EmbeddingEngine, cfg.EmbeddingBaseURL, cfg.EmbeddingAPIKey, cfg.EmbeddingModel,
	)
	if cfg.LLMTimeoutSeconds > 0 {
		llmClient.SetTimeout(time.Duration(cfg.LLMTimeoutSeconds) * time.Second)
	}
	llmClient.SetTranscriptMaxChars(cfg.TranscriptMaxChars)

	limit := *maxChars
	if limit < 0 {
		limit = cfg.TranscriptMaxChars
	}
	model := llmClient.GetOrDiscoverModel(context.Background())

	ytClient := youtube.NewClientWithWhisperURL(llmClient, cfg.WhisperURL, cfg.WhisperBinaryPath, cfg.WhisperModelPath, cfg.YouTubeCookiesPath, cfg.WhisperWindowSeconds, cfg.WhisperMaxWindows)

	fmt.Printf("model: %s @ %s\n", model, cfg.LLMBaseURL)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	fetchStart := time.Now()
	transcript, err := ytClient.FetchTranscript(ctx, *videoID, *durationSec)
	if err != nil {
		log.Fatalf("fetch transcript: %v", err)
	}
	fetchDur := time.Since(fetchStart)
	fmt.Printf("transcript: %d runes, fetched in %s\n", len([]rune(transcript)), fetchDur.Round(time.Millisecond))

	if *dump != "" {
		if err := os.WriteFile(*dump, []byte(transcript), 0644); err != nil {
			log.Printf("dump transcript: %v", err)
		} else {
			fmt.Printf("transcript dumped to %s\n", *dump)
		}
	}

	if *game == "" {
		*game = "(unknown game)"
	}

	for _, s := range strings.Split(*strategiesFlag, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		strategy := youtube.ExcerptStrategy(s)

		excerpt := youtube.SelectTranscriptExcerpt(transcript, strategy, *budget)
		runes := len([]rune(excerpt))

		start := time.Now()
		summary, err := llmClient.SummarizeVideoTranscriptWithLimit(ctx, *game, *channel, *title, excerpt, limit)
		dur := time.Since(start)

		fmt.Printf("\n=== strategy=%s budget=%d maxchars=%d (input=%d runes, llm=%s) ===\n", strategy, *budget, limit, runes, dur.Round(time.Millisecond))
		if err != nil {
			fmt.Printf("ERROR: %v\n", err)
			continue
		}
		fmt.Println(summary)
	}

	_ = os.Stdout.Sync()
}
