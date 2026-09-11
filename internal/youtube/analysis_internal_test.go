package youtube

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"metacrawler/internal/domain"
)

func testVideoInfo() *VideoInfo {
	return &VideoInfo{
		VideoID:     "v1",
		Title:       "Some Game Walkthrough",
		ChannelName: "LetsPlayer",
		URL:         "https://www.youtube.com/watch?v=v1",
		ViewCount:   1000,
	}
}

func TestAnalysisFromTranscript_NoTranscript(t *testing.T) {
	c := NewClientWithWhisperURL(nil, "", "", "", "", 0, 0)

	analysis := c.analysisFromTranscript(context.Background(), "game-1", "Test Game", testVideoInfo(), "", fmt.Errorf("no subtitles available"))

	require.NotNil(t, analysis)
	require.Equal(t, domain.YouTubeStatusNoTranscript, analysis.Status)
	require.Empty(t, analysis.Summary, "без транскрипта вывод по словам блогера не выдумывается")
	require.Equal(t, "v1", analysis.VideoID)
	require.Equal(t, "game-1", analysis.GameID)
}

func TestAnalysisFromTranscript_NoTranscriptEmptyError(t *testing.T) {
	c := NewClientWithWhisperURL(nil, "", "", "", "", 0, 0)

	// Пустой транскрипт без ошибки - тоже отсутствие транскрипта.
	analysis := c.analysisFromTranscript(context.Background(), "game-1", "Test Game", testVideoInfo(), "   ", nil)

	require.NotNil(t, analysis)
	require.Equal(t, domain.YouTubeStatusNoTranscript, analysis.Status)
	require.Empty(t, analysis.Summary)
}

func TestAnalysisFromTranscript_Analyzed(t *testing.T) {
	c := NewClientWithWhisperURL(nil, "", "", "", "", 0, 0)

	transcript := "The combat is great but the game has many bugs."
	analysis := c.analysisFromTranscript(context.Background(), "game-2", "Test Game", testVideoInfo(), transcript, nil)

	require.NotNil(t, analysis)
	require.Equal(t, domain.YouTubeStatusAnalyzed, analysis.Status)
	require.NotEmpty(t, analysis.Summary)
	require.Contains(t, analysis.Summary, "combat")
}
