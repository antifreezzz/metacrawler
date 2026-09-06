package youtube_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"metacrawler/internal/youtube"
)

func TestParseViewCount(t *testing.T) {
	require.Equal(t, int64(1234567), youtube.ParseViewCount("1,234,567 views"))
	require.Equal(t, int64(1500000), youtube.ParseViewCount("1.5M views"))
	require.Equal(t, int64(250000), youtube.ParseViewCount("250K views"))
	require.Equal(t, int64(45), youtube.ParseViewCount("45 views"))
	require.Equal(t, int64(0), youtube.ParseViewCount("No views"))
}

func TestParseTimedTextXML(t *testing.T) {
	xmlData := `<?xml version="1.0" encoding="utf-8" ?>
	<transcript>
		<text start="0.5" dur="2.1">Hello everyone and welcome</text>
		<text start="2.6" dur="3.0">to this gameplay walkthrough of Elden Ring.</text>
		<text start="5.7" dur="2.5">The graphics here are absolutely breathtaking!</text>
	</transcript>`

	text, err := youtube.ParseTimedText([]byte(xmlData))
	require.NoError(t, err)
	require.Contains(t, text, "Hello everyone and welcome")
	require.Contains(t, text, "gameplay walkthrough of Elden Ring")
	require.Contains(t, text, "The graphics here are absolutely breathtaking!")
}

func TestParseSearchResults(t *testing.T) {
	mockHTML := `<!DOCTYPE html><html><body><script>
	var ytInitialData = {
		"contents": {
			"twoColumnSearchResultsRenderer": {
				"primaryContents": {
					"sectionListRenderer": {
						"contents": [{
							"itemSectionRenderer": {
								"contents": [
									{
										"videoRenderer": {
											"videoId": "vid_pocket",
											"title": {"runs": [{"text": "Pocket Ants: Colony Simulator Gameplay"}]},
											"ownerText": {"runs": [{"text": "Mobile Gaming"}]},
											"viewCountText": {"simpleText": "500,000 views"}
										}
									},
									{
										"videoRenderer": {
											"videoId": "vid_ant",
											"title": {"runs": [{"text": "ANT SIMULATOR: stock market game - First Walkthrough"}]},
											"ownerText": {"runs": [{"text": "Indie Explorer"}]},
											"viewCountText": {"simpleText": "12,000 views"}
										}
									}
								]
							}
						}]
					}
				}
			}
		}
	};
	</script></body></html>`

	// 1. Поиск для ANT SIMULATOR должен отбросить Pocket Ants и выбрать vid_ant
	best, err := youtube.ParseTopVideoFromSearchHTML([]byte(mockHTML), "ANT SIMULATOR: stock market game")
	require.NoError(t, err)
	require.NotNil(t, best)
	require.Equal(t, "vid_ant", best.VideoID)
	require.Equal(t, "ANT SIMULATOR: stock market game - First Walkthrough", best.Title)

	// 2. Если игра не совпадает ни с одним видео — возвращается ошибка
	_, errMismatch := youtube.ParseTopVideoFromSearchHTML([]byte(mockHTML), "Escape from Company")
	require.Error(t, errMismatch)
	require.Contains(t, errMismatch.Error(), "no relevant video found")
}

func TestIsVideoRelevant(t *testing.T) {
	// Кейс 1: ANT SIMULATOR не должен матчить Pocket Ants
	require.False(t, youtube.IsVideoRelevant("ANT SIMULATOR: stock market game", "Pocket Ants: Colony Simulator Gameplay Walkthrough (Android, iOS)"))
	require.True(t, youtube.IsVideoRelevant("ANT SIMULATOR: stock market game", "ANT SIMULATOR: stock market game - Gameplay Walkthrough Part 1"))
	require.True(t, youtube.IsVideoRelevant("ANT SIMULATOR: stock market game", "Ant Simulator Gameplay Walkthrough"))

	// Кейс 2: Escape from Company не должен матчить Star Wars Zero Company
	require.False(t, youtube.IsVideoRelevant("Escape from Company", "Star Wars Zero Company Episode 1 Gameplay"))
	require.True(t, youtube.IsVideoRelevant("Escape from Company", "Escape from Company - Full Gameplay Walkthrough"))

	// Кейс 3: Elden Ring
	require.True(t, youtube.IsVideoRelevant("Elden Ring", "Elden Ring Walkthrough Gameplay Part 1"))
	require.False(t, youtube.IsVideoRelevant("Elden Ring", "Dark Souls 3 Boss Fights"))
}

func TestParseTimedTextWebVTT(t *testing.T) {
	vttData := `WEBVTT
Kind: captions
Language: en

00:00:01.360 --> 00:00:03.040
[Music]

00:00:03.100 --> 00:00:06.200
Hello everyone and welcome back to the channel.

00:00:06.250 --> 00:00:09.500
Today we are playing this incredible game.
`
	text, err := youtube.ParseTimedText([]byte(vttData))
	require.NoError(t, err)
	require.Contains(t, text, "Hello everyone and welcome back to the channel.")
	require.Contains(t, text, "Today we are playing this incredible game.")
	require.NotContains(t, text, "[Music]")
	require.NotContains(t, text, "-->")
}

func TestCleanWhisperOutput(t *testing.T) {
	raw := `ggml_vulkan: Found 1 Vulkan devices:
ggml_vulkan: 0 = Intel(R) Arc(tm) A770 Graphics (DG2)
read_audio_data: reading audio data from '/tmp/test.mp3' ...
read_audio_data: trying to decode with miniaudio
whisper_model_load: loading model
system_info: n_threads = 4 / 12

Hello everyone, welcome back to another gaming session!
Today we are diving into Onimusha: Way of the Sword.
main: processing audio done
`
	cleaned := youtube.CleanWhisperOutput(raw)
	require.Equal(t, "Hello everyone, welcome back to another gaming session! Today we are diving into Onimusha: Way of the Sword.", cleaned)
}

