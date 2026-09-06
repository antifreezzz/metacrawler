package config

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port            string
	DBPath          string
	CronSchedule    string
	CrawlDelayMinMs int
	CrawlDelayMaxMs int
	LLMBaseURL         string
	LLMAPIKey          string
	LLMModel           string
	EmbeddingEngine    string // "local" (default) or "remote"
	EmbeddingBaseURL   string
	EmbeddingAPIKey    string
	EmbeddingModel     string
	AdminUsername      string
	AdminPassword      string
	SessionSecret      string
	WhisperURL         string
	WhisperBinaryPath  string
	WhisperModelPath   string
	YouTubeCookiesPath string
}

func Load() *Config {
	loadDotEnv(".env")

	llmBaseURL := getEnv("LLM_BASE_URL", "http://localhost:8080/v1")
	llmAPIKey := getEnv("LLM_API_KEY", "")

	return &Config{
		Port:               getEnv("PORT", "8079"),
		DBPath:             getEnv("DB_PATH", "data/metacrawler.db"),
		CronSchedule:       getEnv("CRON_SCHEDULE", "0 * * * *"),
		CrawlDelayMinMs:    getEnvAsInt("CRAWL_DELAY_MIN_MS", 2000),
		CrawlDelayMaxMs:    getEnvAsInt("CRAWL_DELAY_MAX_MS", 4000),
		LLMBaseURL:         llmBaseURL,
		LLMAPIKey:          llmAPIKey,
		LLMModel:           getEnv("LLM_MODEL", "auto"),
		EmbeddingEngine:    getEnv("EMBEDDING_ENGINE", "local"),
		EmbeddingBaseURL:   getEnv("EMBEDDING_BASE_URL", llmBaseURL),
		EmbeddingAPIKey:    getEnv("EMBEDDING_API_KEY", llmAPIKey),
		EmbeddingModel:     getEnv("EMBEDDING_MODEL", "local"),
		AdminUsername:      getEnv("ADMIN_USERNAME", "admin"),
		AdminPassword:      getEnv("ADMIN_PASSWORD", ""),
		SessionSecret:      getEnv("SESSION_SECRET", "metacrawler-secret-key-change-me"),
		WhisperURL:         getEnv("WHISPER_URL", ""),
		WhisperBinaryPath:  getEnv("WHISPER_BINARY_PATH", "/home/antifreezzz/whisper.cpp/build-vk/bin/whisper-cli"),
		WhisperModelPath:   getEnv("WHISPER_MODEL_PATH", "/home/antifreezzz/whisper.cpp/models/ggml-tiny.bin"),
		YouTubeCookiesPath: getEnv("YOUTUBE_COOKIES_PATH", ""),
	}
}

func loadDotEnv(filepath string) {
	file, err := os.Open(filepath)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getEnvAsInt(key string, defaultVal int) int {
	valStr := os.Getenv(key)
	if valStr == "" {
		return defaultVal
	}
	val, err := strconv.Atoi(valStr)
	if err != nil {
		return defaultVal
	}
	return val
}
