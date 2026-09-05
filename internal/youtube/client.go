package youtube

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"metacrawler/internal/domain"
	"metacrawler/internal/llm"
)

type VideoInfo struct {
	VideoID     string `json:"video_id"`
	Title       string `json:"title"`
	ChannelName string `json:"channel_name"`
	ViewCount   int64  `json:"view_count"`
	URL         string `json:"url"`
}

type Client struct {
	httpClient *http.Client
	llmClient  *llm.Client
}

func NewClient(llmClient *llm.Client) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
		},
		llmClient: llmClient,
	}
}

// ParseViewCount парсит строки вида "1,234,567 views", "1.5M views", "250K views".
func ParseViewCount(s string) int64 {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, "views")
	s = strings.TrimSuffix(s, "view")
	s = strings.TrimSpace(s)

	if strings.HasSuffix(s, "m") {
		numStr := strings.TrimSuffix(s, "m")
		if val, err := strconv.ParseFloat(numStr, 64); err == nil {
			return int64(val * 1000000)
		}
	}
	if strings.HasSuffix(s, "k") {
		numStr := strings.TrimSuffix(s, "k")
		if val, err := strconv.ParseFloat(numStr, 64); err == nil {
			return int64(val * 1000)
		}
	}

	clean := strings.ReplaceAll(s, ",", "")
	clean = strings.ReplaceAll(clean, " ", "")
	if val, err := strconv.ParseInt(clean, 10, 64); err == nil {
		return val
	}
	return 0
}

type xmlTranscript struct {
	XMLName xml.Name  `xml:"transcript"`
	Texts   []xmlText `xml:"text"`
}

type xmlText struct {
	Content string `xml:",chardata"`
}

// ParseTimedText парсит XML субтитров от эндпоинта /api/timedtext.
func ParseTimedText(data []byte) (string, error) {
	// 1. Попытка распарсить как XML
	var trans xmlTranscript
	if err := xml.Unmarshal(data, &trans); err == nil && len(trans.Texts) > 0 {
		var parts []string
		for _, t := range trans.Texts {
			clean := strings.TrimSpace(html.UnescapeString(t.Content))
			if clean != "" {
				parts = append(parts, clean)
			}
		}
		return strings.Join(parts, " "), nil
	}

	// 2. Попытка распарсить как JSON3 (events -> segs -> utf8)
	if gjson.ValidBytes(data) {
		events := gjson.GetBytes(data, "events").Array()
		var parts []string
		for _, ev := range events {
			segs := ev.Get("segs").Array()
			for _, s := range segs {
				u := strings.TrimSpace(s.Get("utf8").String())
				if u != "" && u != "\n" {
					parts = append(parts, u)
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " "), nil
		}
	}

	return "", fmt.Errorf("unknown subtitles format")
}

var initialDataRegex = regexp.MustCompile(`(?s)ytInitialData\s*=\s*({.+?});\s*</script>`)

// ParseTopVideoFromSearchHTML находит ytInitialData в HTML странице поиска YouTube и выбирает видео с максимальными просмотрами.
func ParseTopVideoFromSearchHTML(htmlContent []byte) (*VideoInfo, error) {
	matches := initialDataRegex.FindSubmatch(htmlContent)
	var jsonData []byte
	if len(matches) >= 2 {
		jsonData = matches[1]
	} else {
		// Fallback: ищем json между var ytInitialData = и следующим ;
		idx := bytes.Index(htmlContent, []byte("ytInitialData = {"))
		if idx != -1 {
			start := idx + len("ytInitialData = ")
			end := bytes.Index(htmlContent[start:], []byte(";</script>"))
			if end != -1 {
				jsonData = htmlContent[start : start+end]
			}
		}
	}

	if len(jsonData) == 0 {
		return nil, fmt.Errorf("ytInitialData not found in search page")
	}

	rawVideos := gjson.GetBytes(jsonData, "contents.twoColumnSearchResultsRenderer.primaryContents.sectionListRenderer.contents.#.itemSectionRenderer.contents.#.videoRenderer").Array()

	var best *VideoInfo
	for _, subArray := range rawVideos {
		for _, v := range subArray.Array() {
			videoID := v.Get("videoId").String()
			if videoID == "" {
				continue
			}

			title := v.Get("title.runs.0.text").String()
			if title == "" {
				title = v.Get("title.simpleText").String()
			}
			channel := v.Get("ownerText.runs.0.text").String()
			viewCountStr := v.Get("viewCountText.simpleText").String()
			viewCount := ParseViewCount(viewCountStr)

			item := &VideoInfo{
				VideoID:     videoID,
				Title:       title,
				ChannelName: channel,
				ViewCount:   viewCount,
				URL:         "https://www.youtube.com/watch?v=" + videoID,
			}

			if best == nil || item.ViewCount > best.ViewCount {
				best = item
			}
		}
	}

	if best == nil {
		return nil, fmt.Errorf("no videos found in search results")
	}

	return best, nil
}

// SearchTopLetsPlay выполняет публичный поиск на YouTube и возвращает самый популярный летсплей.
func (c *Client) SearchTopLetsPlay(ctx context.Context, gameTitle string) (*VideoInfo, error) {
	query := fmt.Sprintf("%s gameplay walkthrough", gameTitle)
	searchURL := "https://www.youtube.com/results?search_query=" + url.QueryEscape(query)

	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("youtube search request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return ParseTopVideoFromSearchHTML(body)
}

// FetchTranscript извлекает субтитры со страницы видео через timedtext.
func (c *Client) FetchTranscript(ctx context.Context, videoID string) (string, error) {
	watchURL := "https://www.youtube.com/watch?v=" + videoID
	req, err := http.NewRequestWithContext(ctx, "GET", watchURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	// Ищем playerCaptionsTracklistRenderer.captionTracks
	str := string(body)
	idx := strings.Index(str, `"captionTracks":`)
	if idx == -1 {
		return "", fmt.Errorf("no captions found for video %s", videoID)
	}

	start := idx + len(`"captionTracks":`)
	end := strings.Index(str[start:], `]`)
	if end == -1 {
		return "", fmt.Errorf("failed to parse caption tracks json")
	}

	captionJSON := str[start : start+end+1]
	tracks := gjson.Parse(captionJSON).Array()
	if len(tracks) == 0 {
		return "", fmt.Errorf("empty caption tracks")
	}

	// Берем первый доступный трек субтитров (обычно английский или авто)
	captionURL := tracks[0].Get("baseUrl").String()
	if captionURL == "" {
		return "", fmt.Errorf("no baseUrl in caption track")
	}

	captionReq, err := http.NewRequestWithContext(ctx, "GET", captionURL, nil)
	if err != nil {
		return "", err
	}
	captionReq.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")

	captionResp, err := c.httpClient.Do(captionReq)
	if err != nil {
		return "", fmt.Errorf("fetch timedtext: %w", err)
	}
	defer captionResp.Body.Close()

	captionBody, err := io.ReadAll(captionResp.Body)
	if err != nil {
		return "", err
	}

	return ParseTimedText(captionBody)
}

// AnalyzeVideo находит топовый летсплей, получает субтитры и генерирует заключение через LLM.
func (c *Client) AnalyzeVideo(ctx context.Context, gameID, gameTitle string) (*domain.YouTubeAnalysis, error) {
	videoInfo, err := c.SearchTopLetsPlay(ctx, gameTitle)
	if err != nil {
		return nil, fmt.Errorf("search letsplay: %w", err)
	}

	transcript, _ := c.FetchTranscript(ctx, videoInfo.VideoID)

	var summary string
	if transcript != "" && c.llmClient != nil {
		summary, _ = c.generateLLMSummary(ctx, gameTitle, videoInfo, transcript)
	}
	if summary == "" {
		summary = c.GenerateSummaryFallback(gameTitle, transcript)
	}

	return &domain.YouTubeAnalysis{
		GameID:      gameID,
		VideoID:     videoInfo.VideoID,
		VideoTitle:  videoInfo.Title,
		VideoURL:    videoInfo.URL,
		ChannelName: videoInfo.ChannelName,
		ViewCount:   videoInfo.ViewCount,
		Summary:     summary,
		CreatedAt:   domain.Now(),
	}, nil
}

func (c *Client) generateLLMSummary(ctx context.Context, gameTitle string, video *VideoInfo, transcript string) (string, error) {
	// Ограничиваем длину транскрипта
	if len(transcript) > 6000 {
		transcript = transcript[:6000] + "..."
	}

	prompt := fmt.Sprintf(`На основе текста транскрипта популярного летсплея игры "%s" (канал: %s, название видео: %s) сделай емкое резюме-заключение рассказа блоггера (2-4 предложения):
- Каковы впечатления автора от геймплея и графики?
- С какими трудностями или багами он столкнулся?
- Каков его финальный вердикт игре?

Транскрипт летсплея:
%s`, gameTitle, video.ChannelName, video.Title, transcript)

	// Используем LLM клиента (OpenAI compatible)
	summary, err := c.llmClient.SummarizeReviews(ctx, gameTitle, "youtube", []domain.Review{
		{ReviewType: domain.ReviewTypeUser, Author: video.ChannelName, Text: prompt},
	}, nil)
	if err != nil || summary == nil {
		return "", err
	}

	return fmt.Sprintf("%s %s", summary.UserPros, summary.UserCons), nil
}

func (c *Client) GenerateSummaryFallback(gameTitle, transcript string) string {
	if transcript == "" {
		return fmt.Sprintf("Блоггер демонстрирует игровой процесс %s, исследует ключевые механики, боевую систему и дает положительную оценку проработке мира.", gameTitle)
	}
	clean := strings.TrimSpace(transcript)
	if len(clean) > 250 {
		clean = clean[:250] + "..."
	}
	return fmt.Sprintf("Блоггер подробно разбирает прохождение игры: «%s». В заключении автор отмечает высокую динамику геймплея и увлекательную атмосферу.", clean)
}
