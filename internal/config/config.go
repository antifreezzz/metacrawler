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
	LLMBaseURL      string
	LLMAPIKey       string
	LLMModel        string
	EmbeddingModel  string
}

func Load() *Config {
	loadDotEnv(".env")

	return &Config{
		Port:            getEnv("PORT", "8080"),
		DBPath:          getEnv("DB_PATH", "data/metacrawler.db"),
		CronSchedule:    getEnv("CRON_SCHEDULE", "0 * * * *"),
		CrawlDelayMinMs: getEnvAsInt("CRAWL_DELAY_MIN_MS", 2000),
		CrawlDelayMaxMs: getEnvAsInt("CRAWL_DELAY_MAX_MS", 4000),
		LLMBaseURL:      getEnv("LLM_BASE_URL", "https://api.openai.com/v1"),
		LLMAPIKey:       getEnv("LLM_API_KEY", ""),
		LLMModel:        getEnv("LLM_MODEL", "gpt-4o-mini"),
		EmbeddingModel:  getEnv("EMBEDDING_MODEL", "text-embedding-3-small"),
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
