package config_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"metacrawler/internal/config"
)

func TestLoadConfig_Defaults(t *testing.T) {
	cfg := config.Load()
	require.Equal(t, "8079", cfg.Port)
	require.Equal(t, "data/metacrawler.db", cfg.DBPath)
	require.Equal(t, 2000, cfg.CrawlDelayMinMs)
	require.Equal(t, 4000, cfg.CrawlDelayMaxMs)
}

func TestLoadConfig_EnvOverrides(t *testing.T) {
	os.Setenv("PORT", "9090")
	os.Setenv("DB_PATH", ":memory:")
	os.Setenv("CRAWL_DELAY_MIN_MS", "1500")
	os.Setenv("LLM_TIMEOUT_SECONDS", "120")
	defer func() {
		os.Unsetenv("PORT")
		os.Unsetenv("DB_PATH")
		os.Unsetenv("CRAWL_DELAY_MIN_MS")
		os.Unsetenv("LLM_TIMEOUT_SECONDS")
	}()

	cfg := config.Load()
	require.Equal(t, "9090", cfg.Port)
	require.Equal(t, ":memory:", cfg.DBPath)
	require.Equal(t, 1500, cfg.CrawlDelayMinMs)
	require.Equal(t, 120, cfg.LLMTimeoutSeconds)
}

func TestLoadConfig_LLMTimeoutDefault(t *testing.T) {
	cfg := config.Load()
	require.Equal(t, 45, cfg.LLMTimeoutSeconds)
}

func TestLoadConfig_WhisperSamplingDefaults(t *testing.T) {
	cfg := config.Load()
	require.Equal(t, 60, cfg.WhisperWindowSeconds)
	require.Equal(t, 3, cfg.WhisperMaxWindows)
}

func TestLoadConfig_WhisperSamplingEnvOverrides(t *testing.T) {
	os.Setenv("WHISPER_WINDOW_SECONDS", "90")
	os.Setenv("WHISPER_MAX_WINDOWS", "2")
	defer func() {
		os.Unsetenv("WHISPER_WINDOW_SECONDS")
		os.Unsetenv("WHISPER_MAX_WINDOWS")
	}()

	cfg := config.Load()
	require.Equal(t, 90, cfg.WhisperWindowSeconds)
	require.Equal(t, 2, cfg.WhisperMaxWindows)
}

func TestLoadConfig_TranscriptMaxChars(t *testing.T) {
	cfg := config.Load()
	require.Equal(t, 40000, cfg.TranscriptMaxChars)

	os.Setenv("TRANSCRIPT_MAX_CHARS", "12000")
	defer os.Unsetenv("TRANSCRIPT_MAX_CHARS")

	cfg = config.Load()
	require.Equal(t, 12000, cfg.TranscriptMaxChars)
}
