package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

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
	Duration    string `json:"duration"`
}

type Client struct {
	httpClient        *http.Client
	llmClient         *llm.Client
	whisperURL        string
	whisperBinaryPath string
	whisperModelPath  string
	cookiesPath       string
}

func NewClient(llmClient *llm.Client) *Client {
	return NewClientWithConfig(llmClient, "", "", "")
}

func NewClientWithConfig(llmClient *llm.Client, whisperBinary, whisperModel, cookiesPath string) *Client {
	return NewClientWithWhisperURL(llmClient, "", whisperBinary, whisperModel, cookiesPath)
}

func NewClientWithWhisperURL(llmClient *llm.Client, whisperURL, whisperBinary, whisperModel, cookiesPath string) *Client {
	if whisperBinary == "" && whisperURL == "" {
		for _, p := range []string{
			"/usr/local/bin/whisper-cli",
			"/usr/bin/whisper-cli",
		} {
			if _, err := os.Stat(p); err == nil {
				whisperBinary = p
				break
			}
		}
	}

	if whisperModel == "" && whisperURL == "" {
		for _, p := range []string{
			"models/ggml-tiny.bin",
			"models/ggml-base.bin",
			"/usr/local/share/whisper/models/ggml-tiny.bin",
		} {
			if _, err := os.Stat(p); err == nil {
				whisperModel = p
				break
			}
		}
	}

	return &Client{
		httpClient: &http.Client{
			Timeout: 45 * time.Second,
		},
		llmClient:         llmClient,
		whisperURL:        whisperURL,
		whisperBinaryPath: whisperBinary,
		whisperModelPath:  whisperModel,
		cookiesPath:       cookiesPath,
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

// NormalizeTitle приводит строку к нормализованному виду для сопоставления названий.
func NormalizeTitle(s string) string {
	s = strings.ToLower(s)
	replacer := strings.NewReplacer(
		" viii", " 8",
		" vii", " 7",
		" vi", " 6",
		" iv", " 4",
		" v", " 5",
		" iii", " 3",
		" ii", " 2",
		" i", " 1",
	)
	s = replacer.Replace(" " + s + " ")

	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// minLetsPlaySeconds - минимальная длительность ролика, чтобы считать его летсплеем.
// Трейлеры и промо-ролики, как правило, короче; live-трансляции не имеют lengthText и не отбрасываются.
const minLetsPlaySeconds = 300

// trailerTokens - одиночные слова, однозначно указывающие на трейлер или промо-ролик.
var trailerTokens = map[string]bool{
	"trailer":      true,
	"trailers":     true,
	"teaser":       true,
	"teasers":      true,
	"cinematic":    true,
	"cinematics":   true,
	"announcement": true,
	"preorder":     true,
	"preorders":    true,
}

// trailerPhrases - словосочетания-маркеры промо-роликов (по нормализованным токенам).
var trailerPhrases = []string{
	"gameplay reveal",
	"gameplay demo",
	"tv spot",
	"coming soon",
	"release date",
}

// ParseDuration переводит строку длительности YouTube ("1:02:33", "3:21") в секунды.
// Возвращает 0 для пустых, live-трансляций и нераспознанных значений.
func ParseDuration(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0
	}
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 {
			return 0
		}
		total = total*60 + n
	}
	return total
}

// IsTrailer определяет, является ли ролик трейлером или промо-материалом по заголовку.
// Намеренно не использует широкие слова (official, launch, reveal в одиночку), чтобы не резать летсплеи и стримы.
func IsTrailer(title string) bool {
	words := strings.Fields(NormalizeTitle(title))
	if len(words) == 0 {
		return false
	}
	for _, w := range words {
		if trailerTokens[w] {
			return true
		}
	}
	joined := " " + strings.Join(words, " ") + " "
	for _, p := range trailerPhrases {
		if strings.Contains(joined, " "+p+" ") {
			return true
		}
	}
	return false
}

// IsVideoRelevant проверяет соответствие названия видео названию игры.
// Исключает ложные срабатывания (например, Pocket Ants для ANT SIMULATOR или Star Wars для Escape from Company).
func IsVideoRelevant(gameTitle, videoTitle string) bool {
	normGame := NormalizeTitle(gameTitle)
	normVideo := NormalizeTitle(videoTitle)
	if normGame == "" || normVideo == "" {
		return false
	}

	// 1. Полное вхождение названия игры в название видео
	if strings.Contains(normVideo, normGame) {
		return true
	}

	// 2. Определение основного названия (до двоеточия или тире, например "ANT SIMULATOR: stock market game")
	primaryGame := gameTitle
	for _, sep := range []string{":", " - ", "—", "–", "|"} {
		if idx := strings.Index(primaryGame, sep); idx != -1 {
			candidate := strings.TrimSpace(primaryGame[:idx])
			if len(candidate) >= 3 {
				primaryGame = candidate
				break
			}
		}
	}
	normPrimary := NormalizeTitle(primaryGame)
	if normPrimary != "" && strings.Contains(normVideo, normPrimary) {
		return true
	}

	// 3. Анализ по ключевым токенам
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "of": true, "in": true,
		"on": true, "at": true, "to": true, "for": true, "from": true,
		"and": true, "or": true, "by": true, "is": true, "with": true,
		"game": true, "games": true, "edition": true, "deluxe": true,
	}

	rawTokens := strings.Fields(normPrimary)
	var gameTokens []string
	for _, t := range rawTokens {
		if !stopWords[t] && len(t) >= 2 {
			gameTokens = append(gameTokens, t)
		}
	}

	if len(gameTokens) == 0 {
		gameTokens = rawTokens
	}
	if len(gameTokens) == 0 {
		return false
	}

	videoWords := make(map[string]bool)
	for _, w := range strings.Fields(normVideo) {
		videoWords[w] = true
	}

	matchedTokens := 0
	for _, t := range gameTokens {
		if videoWords[t] {
			matchedTokens++
		}
	}

	// Если в названии 1 или 2 ключевых слова (например, "escape", "company" или "ant", "simulator"),
	// ВСЕ ключевые слова должны присутствовать в названии видео
	if len(gameTokens) <= 2 {
		return matchedTokens == len(gameTokens)
	}

	// Если 3 и более ключевых слов — должно совпадать не менее 70% и первое ключевое слово
	firstTokenMatched := videoWords[gameTokens[0]]
	ratio := float64(matchedTokens) / float64(len(gameTokens))
	return firstTokenMatched && ratio >= 0.7
}

type xmlTranscript struct {
	XMLName xml.Name  `xml:"transcript"`
	Texts   []xmlText `xml:"text"`
}

type xmlText struct {
	Content string `xml:",chardata"`
}

var (
	tagRegex           = regexp.MustCompile(`<[^>]+>`)
	timestampLineRegex = regexp.MustCompile(`^\s*(?:\d{1,2}:)?\d{2}:\d{2}(?:\.\d{1,3}|,\d{1,3})\s*-->\s*(?:\d{1,2}:)?\d{2}:\d{2}(?:\.\d{1,3}|,\d{1,3})`)
	soundRegex         = regexp.MustCompile(`^\[[^\]]+\]$`)
	initialDataRegex   = regexp.MustCompile(`(?s)ytInitialData\s*=\s*({.+?});\s*</script>`)
)

// ParseTimedText парсит субтитры из форматов WebVTT, SRT, XML и JSON3.
func ParseTimedText(data []byte) (string, error) {
	str := string(data)
	trimmed := strings.TrimSpace(str)

	// 1. WebVTT или SRT
	if strings.HasPrefix(trimmed, "WEBVTT") || timestampLineRegex.MatchString(str) {
		lines := strings.Split(str, "\n")
		var cleanLines []string
		var lastLine string

		for _, rawLine := range lines {
			line := strings.TrimSpace(rawLine)
			if line == "" || strings.HasPrefix(line, "WEBVTT") || strings.HasPrefix(line, "Kind:") || strings.HasPrefix(line, "Language:") || strings.HasPrefix(line, "NOTE") {
				continue
			}
			if timestampLineRegex.MatchString(line) {
				continue
			}
			// Пропуск числовых индексов субтитров
			if _, err := strconv.Atoi(line); err == nil && len(line) < 6 {
				continue
			}

			// Очистка HTML тегов
			line = tagRegex.ReplaceAllString(line, "")
			line = strings.TrimSpace(html.UnescapeString(line))

			// Пропуск шумов и музыки
			if line == "" || soundRegex.MatchString(line) || strings.HasPrefix(line, "♪") || strings.HasSuffix(line, "♪") {
				continue
			}

			// Дедупликация идентичных строк
			if line == lastLine {
				continue
			}
			// Дедупликация нарастающих субтитров YouTube ASR
			if lastLine != "" && strings.HasPrefix(line, lastLine) {
				if len(cleanLines) > 0 {
					cleanLines[len(cleanLines)-1] = line
					lastLine = line
					continue
				}
			}

			cleanLines = append(cleanLines, line)
			lastLine = line
		}

		if len(cleanLines) > 0 {
			return strings.Join(cleanLines, " "), nil
		}
	}

	// 2. XML (<transcript> или <timedtext>)
	if strings.Contains(str, "<transcript") || strings.Contains(str, "<timedtext") {
		var trans xmlTranscript
		if err := xml.Unmarshal(data, &trans); err == nil && len(trans.Texts) > 0 {
			var parts []string
			var last string
			for _, t := range trans.Texts {
				clean := strings.TrimSpace(html.UnescapeString(t.Content))
				if clean != "" && clean != last {
					parts = append(parts, clean)
					last = clean
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, " "), nil
			}
		}
	}

	// 3. JSON3 (events -> segs -> utf8)
	if gjson.ValidBytes(data) {
		events := gjson.GetBytes(data, "events").Array()
		var parts []string
		var last string
		for _, ev := range events {
			segs := ev.Get("segs").Array()
			for _, s := range segs {
				u := strings.TrimSpace(s.Get("utf8").String())
				if u != "" && u != "\n" && u != last {
					parts = append(parts, u)
					last = u
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " "), nil
		}
	}

	return "", fmt.Errorf("unknown subtitles format")
}

// ParseTopVideoFromSearchHTML находит ytInitialData в HTML странице поиска YouTube и выбирает наиболее релевантное видео с максимальными просмотрами.
func ParseTopVideoFromSearchHTML(htmlContent []byte, gameTitle string) (*VideoInfo, error) {
	matches := initialDataRegex.FindSubmatch(htmlContent)
	var jsonData []byte
	if len(matches) >= 2 {
		jsonData = matches[1]
	} else {
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
			if title == "" {
				continue
			}

			// Фильтрация: отбрасываем видео, не относящиеся к игре
			if gameTitle != "" && !IsVideoRelevant(gameTitle, title) {
				continue
			}

			// Фильтрация трейлеров и промо-роликов, не являющихся летсплеями
			if IsTrailer(title) {
				continue
			}
			durationStr := v.Get("lengthText.simpleText").String()
			if secs := ParseDuration(durationStr); secs > 0 && secs < minLetsPlaySeconds {
				continue
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
				Duration:    durationStr,
			}

			if best == nil || item.ViewCount > best.ViewCount {
				best = item
			}
		}
	}

	if best == nil {
		if gameTitle != "" {
			return nil, fmt.Errorf("no relevant video found for %q", gameTitle)
		}
		return nil, fmt.Errorf("no videos found in search results")
	}

	return best, nil
}

// SearchTopLetsPlay выполняет публичный поиск на YouTube и возвращает самый популярный релевантный летсплей.
func (c *Client) SearchTopLetsPlay(ctx context.Context, gameTitle string) (*VideoInfo, error) {
	query := fmt.Sprintf("%s gameplay walkthrough", gameTitle)
	searchURL := "https://www.youtube.com/results?search_query=" + url.QueryEscape(query)

	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/133.0.0.0")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9,ru;q=0.8")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("youtube search request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return ParseTopVideoFromSearchHTML(body, gameTitle)
}

func (c *Client) getYtDlpCommonArgs() []string {
	var args []string
	if nodePath, err := exec.LookPath("node"); err == nil && nodePath != "" {
		args = append(args, "--js-runtimes", "node")
	}

	if c.cookiesPath != "" {
		if _, err := os.Stat(c.cookiesPath); err == nil {
			args = append(args, "--cookies", c.cookiesPath)
		}
	} else {
		// Автоопределение firefox cookies на Linux
		home, _ := os.UserHomeDir()
		if home != "" {
			ffDir := filepath.Join(home, ".mozilla", "firefox")
			if _, err := os.Stat(ffDir); err == nil {
				args = append(args, "--cookies-from-browser", "firefox")
			}
		}
	}
	return args
}

var validVideoIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{3,64}$`)

func (c *Client) findYtDlp() string {
	ytDlpPath, err := exec.LookPath("yt-dlp")
	if err == nil && ytDlpPath != "" {
		return ytDlpPath
	}
	for _, p := range []string{"/usr/bin/yt-dlp", "/usr/local/bin/yt-dlp"} {
		if _, statErr := os.Stat(p); statErr == nil {
			return p
		}
	}
	return ""
}

// fetchViaYtDlp пытается загрузить субтитры через утилиту yt-dlp (если доступна в системе).
func (c *Client) fetchViaYtDlp(ctx context.Context, videoID string) (string, error) {
	if !validVideoIDRe.MatchString(videoID) {
		return "", fmt.Errorf("invalid video ID format")
	}

	ytDlpPath := c.findYtDlp()
	if ytDlpPath == "" {
		return "", fmt.Errorf("yt-dlp not found")
	}

	tmpPattern := filepath.Join(os.TempDir(), fmt.Sprintf("mc_sub_%s_%%(id)s", videoID))
	cmdCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()

	args := []string{
		"--skip-download",
		"--write-sub",
		"--write-auto-sub",
		"--sub-lang", "ru.*,en.*,ru,en",
		"--convert-subs", "vtt",
		"--no-warnings",
	}
	args = append(args, c.getYtDlpCommonArgs()...)
	args = append(args, "-o", tmpPattern, "https://www.youtube.com/watch?v="+videoID)

	cmd := exec.CommandContext(cmdCtx, ytDlpPath, args...)
	_ = cmd.Run()

	globPattern := filepath.Join(os.TempDir(), fmt.Sprintf("mc_sub_%s_*", videoID))
	matches, _ := filepath.Glob(globPattern)
	if len(matches) == 0 {
		return "", fmt.Errorf("no subtitles file generated by yt-dlp")
	}

	var subContent []byte
	for _, m := range matches {
		if subContent == nil && (strings.HasSuffix(m, ".vtt") || strings.HasSuffix(m, ".srt")) {
			if data, err := os.ReadFile(m); err == nil && len(data) > 0 {
				subContent = data
			}
		}
		_ = os.Remove(m)
	}

	if len(subContent) == 0 {
		return "", fmt.Errorf("empty subtitle file from yt-dlp")
	}

	return ParseTimedText(subContent)
}

// fetchViaInnertube загружает субтитры через Innertube Player API (Android VR client).
func (c *Client) fetchViaInnertube(ctx context.Context, videoID string) (string, error) {
	apiURL := "https://www.youtube.com/youtubei/v1/player"
	payload := map[string]interface{}{
		"context": map[string]interface{}{
			"client": map[string]interface{}{
				"clientName":    "ANDROID_VR",
				"clientVersion": "1.60.19",
				"deviceMake":    "Oculus",
				"deviceModel":   "Quest 3",
				"hl":            "en",
				"gl":            "US",
			},
		},
		"videoId": videoID,
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Android; Mobile; rv:133.0) Gecko/133.0 Firefox/133.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	tracks := gjson.GetBytes(respBody, "captions.playerCaptionsTracklistRenderer.captionTracks").Array()
	if len(tracks) == 0 {
		return "", fmt.Errorf("no caption tracks in innertube response")
	}

	var bestURL string
	for _, track := range tracks {
		lang := track.Get("languageCode").String()
		u := track.Get("baseUrl").String()
		if strings.HasPrefix(lang, "ru") || strings.HasPrefix(lang, "en") {
			bestURL = u
			break
		}
		if bestURL == "" {
			bestURL = u
		}
	}

	if bestURL == "" {
		return "", fmt.Errorf("no baseUrl in caption track")
	}

	subReq, err := http.NewRequestWithContext(ctx, "GET", bestURL, nil)
	if err != nil {
		return "", err
	}
	subReq.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64)")

	subResp, err := c.httpClient.Do(subReq)
	if err != nil {
		return "", err
	}
	defer subResp.Body.Close()

	subData, err := io.ReadAll(subResp.Body)
	if err != nil {
		return "", err
	}

	return ParseTimedText(subData)
}

func (c *Client) fetchViaWebPage(ctx context.Context, videoID string) (string, error) {
	watchURL := "https://www.youtube.com/watch?v=" + videoID
	req, err := http.NewRequestWithContext(ctx, "GET", watchURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/133.0.0.0")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9,ru;q=0.8")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	str := string(body)
	idx := strings.Index(str, `"captionTracks":`)
	if idx == -1 {
		return "", fmt.Errorf("no captions found in watch page for video %s", videoID)
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

	captionURL := tracks[0].Get("baseUrl").String()
	if captionURL == "" {
		return "", fmt.Errorf("no baseUrl in caption track")
	}

	captionReq, err := http.NewRequestWithContext(ctx, "GET", captionURL, nil)
	if err != nil {
		return "", err
	}
	captionReq.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64)")

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

func (c *Client) fetchViaWhisper(ctx context.Context, videoID string) (string, error) {
	if !validVideoIDRe.MatchString(videoID) {
		return "", fmt.Errorf("invalid video ID format")
	}

	if c.whisperURL == "" {
		if c.whisperBinaryPath == "" || c.whisperModelPath == "" {
			return "", fmt.Errorf("whisper binary or model not configured")
		}
		if _, err := os.Stat(c.whisperBinaryPath); err != nil {
			return "", fmt.Errorf("whisper binary not found at %s: %w", c.whisperBinaryPath, err)
		}
		if _, err := os.Stat(c.whisperModelPath); err != nil {
			return "", fmt.Errorf("whisper model not found at %s: %w", c.whisperModelPath, err)
		}
	}

	ytDlpPath := c.findYtDlp()
	if ytDlpPath == "" {
		return "", fmt.Errorf("yt-dlp not found")
	}

	tmpAudio := filepath.Join(os.TempDir(), fmt.Sprintf("mc_audio_%s.mp3", videoID))
	defer os.Remove(tmpAudio)

	dlCtx, dlCancel := context.WithTimeout(ctx, 45*time.Second)
	defer dlCancel()

	dlArgs := []string{
		"-f", "ba",
		"--download-sections", "*00:00-01:00",
		"-x",
		"--audio-format", "mp3",
		"--no-warnings",
	}
	dlArgs = append(dlArgs, c.getYtDlpCommonArgs()...)
	dlArgs = append(dlArgs, "-o", tmpAudio, "https://www.youtube.com/watch?v="+videoID)

	dlCmd := exec.CommandContext(dlCtx, ytDlpPath, dlArgs...)
	_ = dlCmd.Run()

	if stat, err := os.Stat(tmpAudio); err != nil || stat.Size() == 0 {
		return "", fmt.Errorf("audio file was not downloaded")
	}

	sttCtx, sttCancel := context.WithTimeout(ctx, 60*time.Second)
	defer sttCancel()

	if c.whisperURL != "" {
		return c.TranscribeViaHTTP(sttCtx, tmpAudio)
	}

	sttCmd := exec.CommandContext(sttCtx, c.whisperBinaryPath,
		"-m", c.whisperModelPath,
		"-f", tmpAudio,
		"-l", "auto",
		"-nt",
		"--no-prints",
	)

	outBytes, err := sttCmd.Output()
	if err != nil {
		return "", fmt.Errorf("whisper execution: %w", err)
	}

	cleaned := CleanWhisperOutput(string(outBytes))
	if len(cleaned) < 10 {
		return "", fmt.Errorf("whisper produced no meaningful text")
	}

	return cleaned, nil
}

func (c *Client) TranscribeViaHTTP(ctx context.Context, audioPath string) (string, error) {
	file, err := os.Open(audioPath)
	if err != nil {
		return "", fmt.Errorf("open audio file for STT: %w", err)
	}
	defer file.Close()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	part, err := writer.CreateFormFile("file", filepath.Base(audioPath))
	if err != nil {
		return "", fmt.Errorf("create multipart form file: %w", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		return "", fmt.Errorf("copy audio to multipart: %w", err)
	}

	_ = writer.WriteField("language", "auto")
	_ = writer.WriteField("model", "whisper-1")

	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.whisperURL, &body)
	if err != nil {
		return "", fmt.Errorf("create STT HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("send STT request to %s: %w", c.whisperURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("STT request failed with status %d: %s", resp.StatusCode, string(respBytes))
	}

	var res struct {
		Text  string `json:"text"`
		Error string `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", fmt.Errorf("decode STT response: %w", err)
	}

	if res.Error != "" {
		return "", fmt.Errorf("STT error: %s", res.Error)
	}

	cleaned := CleanWhisperOutput(res.Text)
	if len(cleaned) < 10 {
		return "", fmt.Errorf("whisper produced no meaningful text")
	}

	return cleaned, nil
}

func CleanWhisperOutput(raw string) string {
	lines := strings.Split(raw, "\n")
	var result []string
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "ggml_") ||
			strings.HasPrefix(trimmed, "read_audio_data:") ||
			strings.HasPrefix(trimmed, "whisper_") ||
			strings.HasPrefix(trimmed, "system_info:") ||
			strings.HasPrefix(trimmed, "main:") {
			continue
		}
		result = append(result, trimmed)
	}
	return strings.Join(result, " ")
}

// FetchTranscript извлекает субтитры к видео, пробуя yt-dlp, Innertube API, веб-страницу и Vulkan Whisper STT.
func (c *Client) FetchTranscript(ctx context.Context, videoID string) (string, error) {
	// 1. Приоритет: yt-dlp (субтитры и автосубтитры)
	if text, err := c.fetchViaYtDlp(ctx, videoID); err == nil && text != "" {
		return text, nil
	}

	// 2. Innertube API
	if text, err := c.fetchViaInnertube(ctx, videoID); err == nil && text != "" {
		return text, nil
	}

	// 3. Fallback: разбор веб-страницы видео
	if text, err := c.fetchViaWebPage(ctx, videoID); err == nil && text != "" {
		return text, nil
	}

	// 4. Локальный Vulkan Whisper STT фолбэк (разбор аудиодорожки летсплея)
	if text, err := c.fetchViaWhisper(ctx, videoID); err == nil && text != "" {
		return text, nil
	}

	return "", fmt.Errorf("no subtitles or voice transcript available for video %s", videoID)
}

// AnalyzeVideo находит топовый релевантный летсплей, получает субтитры и генерирует заключение на основе слов блогера.
func (c *Client) AnalyzeVideo(ctx context.Context, gameID, gameTitle string) (*domain.YouTubeAnalysis, error) {
	videoInfo, err := c.SearchTopLetsPlay(ctx, gameTitle)
	if err != nil {
		return nil, fmt.Errorf("search letsplay: %w", err)
	}

	transcript, _ := c.FetchTranscript(ctx, videoInfo.VideoID)

	var summary string
	if transcript != "" && c.llmClient != nil {
		summary, _ = c.llmClient.SummarizeVideoTranscript(ctx, gameTitle, videoInfo.ChannelName, videoInfo.Title, transcript)
	}
	if summary == "" && transcript != "" {
		summary = generateFallbackTranscriptSummary(gameTitle, videoInfo.ChannelName, transcript)
	}
	if summary == "" {
		summary = fmt.Sprintf("Летсплей от канала %s (голосовые комментарии и субтитры к видео отсутствуют).", videoInfo.ChannelName)
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

func generateFallbackTranscriptSummary(gameTitle, channelName, transcript string) string {
	clean := strings.TrimSpace(transcript)
	if len(clean) > 300 {
		clean = clean[:300] + "..."
	}
	if channelName != "" {
		return fmt.Sprintf("Блогер (%s) проходит игру %s и комментирует происходящее: «%s».", channelName, gameTitle, clean)
	}
	return fmt.Sprintf("В летсплее по игре %s блогер отмечает: «%s».", gameTitle, clean)
}

