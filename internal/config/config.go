package config

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port                  string
	DBPath                string
	CronSchedule          string
	CrawlDelayMinMs       int
	CrawlDelayMaxMs       int
	LLMTimeoutSeconds     int // таймаут запросов к LLM; локальные модели могут думать дольше дефолта
	LLMBaseURL            string
	LLMAPIKey             string
	LLMModel              string
	TranscriptMaxChars    int    // лимит символов транскрипта летсплея перед отправкой в LLM
	EmbeddingEngine       string // "local" (default) or "remote"
	EmbeddingBaseURL      string
	EmbeddingAPIKey       string
	EmbeddingModel        string
	AdminUsername         string
	AdminPassword         string
	SessionSecret         string
	AllowInsecureDefaults bool // разрешить слабые секреты (только для локальной разработки)
	WhisperURL            string
	WhisperBinaryPath     string
	WhisperModelPath      string
	WhisperWindowSeconds  int // длина одного окна аудио для Whisper-фолбэка
	WhisperMaxWindows     int // максимум окон (начало/середина/конец)
	YouTubeCookiesPath    string
}

func Load() *Config {
	loadDotEnv(".env")

	llmBaseURL := getEnv("LLM_BASE_URL", "http://localhost:8080/v1")
	llmAPIKey := getEnv("LLM_API_KEY", "")

	return &Config{
		Port:                  getEnv("PORT", "8079"),
		DBPath:                getEnv("DB_PATH", "data/metacrawler.db"),
		CronSchedule:          getEnv("CRON_SCHEDULE", "0 * * * *"),
		CrawlDelayMinMs:       getEnvAsInt("CRAWL_DELAY_MIN_MS", 2000),
		CrawlDelayMaxMs:       getEnvAsInt("CRAWL_DELAY_MAX_MS", 4000),
		LLMTimeoutSeconds:     getEnvAsInt("LLM_TIMEOUT_SECONDS", 45),
		LLMBaseURL:            llmBaseURL,
		LLMAPIKey:             llmAPIKey,
		LLMModel:              getEnv("LLM_MODEL", "auto"),
		TranscriptMaxChars:    getEnvAsInt("TRANSCRIPT_MAX_CHARS", 40000),
		EmbeddingEngine:       getEnv("EMBEDDING_ENGINE", "local"),
		EmbeddingBaseURL:      getEnv("EMBEDDING_BASE_URL", llmBaseURL),
		EmbeddingAPIKey:       getEnv("EMBEDDING_API_KEY", llmAPIKey),
		EmbeddingModel:        getEnv("EMBEDDING_MODEL", "local"),
		AdminUsername:         getEnv("ADMIN_USERNAME", "admin"),
		AdminPassword:         getEnv("ADMIN_PASSWORD", ""),
		SessionSecret:         getEnv("SESSION_SECRET", "metacrawler-secret-key-change-me"),
		AllowInsecureDefaults: getEnvAsBool("ALLOW_INSECURE_DEFAULTS", false),
		WhisperURL:            getEnv("WHISPER_URL", ""),
		WhisperBinaryPath:     getEnv("WHISPER_BINARY_PATH", ""),
		WhisperModelPath:      getEnv("WHISPER_MODEL_PATH", ""),
		WhisperWindowSeconds:  getEnvAsInt("WHISPER_WINDOW_SECONDS", 60),
		WhisperMaxWindows:     getEnvAsInt("WHISPER_MAX_WINDOWS", 3),
		YouTubeCookiesPath:    getEnv("YOUTUBE_COOKIES_PATH", ""),
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

// Слабые значения, которые нельзя использовать в проде.
var weakAdminPasswords = map[string]bool{
	"admin": true, "password": true, "changeme": true, "change-me": true,
	"metacrawler": true, "123456": true, "12345678": true, "qwerty": true,
	"letmein": true, "test": true,
}

var insecureSessionSecrets = map[string]bool{
	"metacrawler-secret-key-change-me":     true,
	"metacrawler-secret-key":               true,
	"metacrawler-default-secret-key-12345": true,
}

// Validate не дает стартовать сервису со слабыми или отсутствующими секретами.
// Пустой ADMIN_PASSWORD раньше полностью отключал авторизацию админки.
func (c *Config) Validate() error {
	if c.AllowInsecureDefaults {
		return nil
	}

	password := strings.TrimSpace(c.AdminPassword)
	if password == "" {
		return errors.New("ADMIN_PASSWORD is required (set a strong password or ALLOW_INSECURE_DEFAULTS=true for local dev)")
	}
	if len([]rune(password)) < 8 {
		return errors.New("ADMIN_PASSWORD must be at least 8 characters")
	}
	if weakAdminPasswords[strings.ToLower(password)] {
		return errors.New("ADMIN_PASSWORD is a known weak value")
	}

	secret := strings.TrimSpace(c.SessionSecret)
	if secret == "" {
		return errors.New("SESSION_SECRET is required (set a random secret or ALLOW_INSECURE_DEFAULTS=true for local dev)")
	}
	if len([]rune(secret)) < 32 {
		return errors.New("SESSION_SECRET must be at least 32 characters")
	}
	if insecureSessionSecrets[secret] {
		return errors.New("SESSION_SECRET is a known default value")
	}

	return nil
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

func getEnvAsBool(key string, defaultVal bool) bool {
	valStr := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if valStr == "" {
		return defaultVal
	}
	switch valStr {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return defaultVal
	}
}
