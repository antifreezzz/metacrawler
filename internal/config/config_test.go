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
