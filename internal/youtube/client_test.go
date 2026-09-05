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
											"videoId": "vid1",
											"title": {"runs": [{"text": "Gameplay Walkthrough 1"}]},
											"ownerText": {"runs": [{"text": "Channel One"}]},
											"viewCountText": {"simpleText": "50,000 views"}
										}
									},
									{
										"videoRenderer": {
											"videoId": "vid2",
											"title": {"runs": [{"text": "Most Popular Walkthrough"}]},
											"ownerText": {"runs": [{"text": "Big Streamer"}]},
											"viewCountText": {"simpleText": "1,200,000 views"}
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

	best, err := youtube.ParseTopVideoFromSearchHTML([]byte(mockHTML))
	require.NoError(t, err)
	require.NotNil(t, best)
	require.Equal(t, "vid2", best.VideoID)
	require.Equal(t, "Most Popular Walkthrough", best.Title)
	require.Equal(t, "Big Streamer", best.ChannelName)
	require.Equal(t, int64(1200000), best.ViewCount)
	require.Equal(t, "https://www.youtube.com/watch?v=vid2", best.URL)
}

func TestSummarizeFallback(t *testing.T) {
	transcript := "The boss fights are extremely punishing but the world design is pure perfection. Overall a masterpiece."
	client := youtube.NewClient(nil)
	summary := client.GenerateSummaryFallback("Elden Ring", transcript)
	require.NotEmpty(t, summary)
	require.Contains(t, summary, "Блоггер")
}
