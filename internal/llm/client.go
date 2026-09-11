package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"metacrawler/internal/domain"
)

type SummaryResult struct {
	CriticPros string `json:"critic_pros"`
	CriticCons string `json:"critic_cons"`
	UserPros   string `json:"user_pros"`
	UserCons   string `json:"user_cons"`
}

// ErrLLMUnavailable означает, что резюме сгенерировать нельзя: LLM не подключена
// или ответила ошибкой. Выдуманный текст в этом случае запрещен (честность данных).
var ErrLLMUnavailable = errors.New("llm unavailable: summary cannot be generated")

type Client struct {
	baseURL            string
	apiKey             string
	chatModel          string
	embeddingEngine    string // "local" or "remote"
	embeddingBaseURL   string
	embeddingAPIKey    string
	embeddingModel     string
	transcriptMaxChars int
	httpClient         *http.Client
	vectorizer         *Vectorizer
}

func NewClient(baseURL, apiKey, chatModel, embeddingModel string) *Client {
	return NewClientWithEmbedding(baseURL, apiKey, chatModel, "", baseURL, apiKey, embeddingModel)
}

func NewClientWithEmbedding(baseURL, apiKey, chatModel, embeddingEngine, embeddingBaseURL, embeddingAPIKey, embeddingModel string) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(baseURL, "/v1") {
		baseURL += "/v1"
	}

	if embeddingBaseURL == "" {
		embeddingBaseURL = baseURL
	} else {
		embeddingBaseURL = strings.TrimRight(embeddingBaseURL, "/")
		if !strings.HasSuffix(embeddingBaseURL, "/v1") {
			embeddingBaseURL += "/v1"
		}
	}

	if embeddingEngine == "" {
		if embeddingModel == "" || embeddingModel == "local" || embeddingModel == "offline" {
			embeddingEngine = "local"
		} else {
			embeddingEngine = "remote"
		}
	}

	return &Client{
		baseURL:            baseURL,
		apiKey:             apiKey,
		chatModel:          chatModel,
		embeddingEngine:    embeddingEngine,
		embeddingBaseURL:   embeddingBaseURL,
		embeddingAPIKey:    embeddingAPIKey,
		embeddingModel:     embeddingModel,
		transcriptMaxChars: defaultTranscriptMaxChars,
		vectorizer:         NewVectorizer(256),
		httpClient: &http.Client{
			Timeout: 45 * time.Second,
		},
	}
}

// SetTimeout переопределяет таймаут HTTP-запросов к LLM.
func (c *Client) SetTimeout(d time.Duration) {
	if c.httpClient != nil {
		c.httpClient.Timeout = d
	}
}

// SetTranscriptMaxChars задает лимит символов транскрипта летсплея перед отправкой в LLM.
// n <= 0 отключает обрезку.
func (c *Client) SetTranscriptMaxChars(n int) {
	if c != nil {
		c.transcriptMaxChars = n
	}
}

// HasAPIKey возвращает true, если клиент готов отправлять запросы в LLM
// (указан API-ключ либо используется локальный эндпоинт вроде llamacpp/ollama).
func (c *Client) HasAPIKey() bool {
	if c == nil || c.baseURL == "" {
		return false
	}
	if c.apiKey != "" {
		return true
	}
	// Локальные эндпоинты не требуют обязательного API-ключа
	return strings.Contains(c.baseURL, "localhost") ||
		strings.Contains(c.baseURL, "127.0.0.1") ||
		strings.Contains(c.baseURL, "host.docker.internal") ||
		strings.Contains(c.baseURL, ":8080") ||
		strings.Contains(c.baseURL, ":11434") ||
		strings.Contains(c.baseURL, ":1234")
}

func (c *Client) ChatModel() string {
	if c == nil {
		return ""
	}
	return c.chatModel
}

func (c *Client) EmbeddingModel() string {
	if c == nil {
		return ""
	}
	return c.embeddingModel
}

// sendRequest выполняет HTTP-запрос к LLM API с поддержкой Docker host.docker.internal фолбэка.
func (c *Client) sendRequest(ctx context.Context, method, path string, body []byte) (*http.Response, []byte, error) {
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("create http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil && (strings.Contains(url, "localhost") || strings.Contains(url, "127.0.0.1")) {
		// Попытка связаться с хостом из Docker через host.docker.internal
		altURL := strings.Replace(url, "localhost", "host.docker.internal", 1)
		altURL = strings.Replace(altURL, "127.0.0.1", "host.docker.internal", 1)
		if altReq, errAlt := http.NewRequestWithContext(ctx, method, altURL, bytes.NewReader(body)); errAlt == nil {
			altReq.Header = req.Header.Clone()
			if altResp, altErr := c.httpClient.Do(altReq); altErr == nil {
				resp = altResp
				err = nil
			}
		}
	}
	if err != nil {
		return nil, nil, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read response body: %w", err)
	}

	return resp, respBody, nil
}

// ListAvailableModels возвращает список ID моделей, зарегистрированных на LLM-сервере.
func (c *Client) ListAvailableModels(ctx context.Context) ([]string, error) {
	if c == nil || c.baseURL == "" {
		return nil, fmt.Errorf("client is nil or baseURL is empty")
	}

	resp, respBody, err := c.sendRequest(ctx, "GET", "/models", nil)
	if err != nil {
		return nil, fmt.Errorf("execute models request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models status %d: %s", resp.StatusCode, string(respBody))
	}

	var modelIDs []string
	gjson.GetBytes(respBody, "data").ForEach(func(_, value gjson.Result) bool {
		if id := value.Get("id").String(); id != "" {
			modelIDs = append(modelIDs, id)
		}
		return true
	})

	if len(modelIDs) == 0 {
		gjson.GetBytes(respBody, "models").ForEach(func(_, value gjson.Result) bool {
			name := value.Get("name").String()
			if name == "" {
				name = value.Get("model").String()
			}
			if name != "" {
				modelIDs = append(modelIDs, name)
			}
			return true
		})
	}

	return modelIDs, nil
}

// GetOrDiscoverModel возвращает эффективное имя модели:
// если задана конкретная модель (не "auto", "any", ""), возвращает её;
// если "auto", "any" или пусто — опрашивает GET /models и берет любую запущенную там модель.
func (c *Client) GetOrDiscoverModel(ctx context.Context) string {
	if c == nil {
		return "default"
	}
	if c.chatModel != "" && c.chatModel != "auto" && c.chatModel != "any" && c.chatModel != "default" {
		return c.chatModel
	}

	discCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	models, err := c.ListAvailableModels(discCtx)
	if err == nil && len(models) > 0 {
		return models[0]
	}
	if c.chatModel != "" {
		return c.chatModel
	}
	return "default"
}

func cleanJSONMarkdown(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
		if idx := strings.LastIndex(s, "```"); idx != -1 {
			s = s[:idx]
		}
	} else if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		if idx := strings.LastIndex(s, "```"); idx != -1 {
			s = s[:idx]
		}
	}
	return strings.TrimSpace(s)
}

// SummarizeReviews отправляет отзывы критиков и игроков в LLM для формирования раздельного резюме плюсов и минусов.
// При недоступности LLM возвращается ErrLLMUnavailable: фейковые резюме не генерируются.
func (c *Client) SummarizeReviews(ctx context.Context, gameTitle, platform string, criticReviews, userReviews []domain.Review) (*SummaryResult, error) {
	if !c.HasAPIKey() {
		return nil, fmt.Errorf("%w: LLM_API_KEY не задан", ErrLLMUnavailable)
	}

	var criticText strings.Builder
	for i, r := range criticReviews {
		if i >= 10 {
			break
		}
		criticText.WriteString(fmt.Sprintf("- [%s] %s\n", r.Author, r.Text))
	}

	var userText strings.Builder
	for i, r := range userReviews {
		if i >= 10 {
			break
		}
		userText.WriteString(fmt.Sprintf("- [%s] %s\n", r.Author, r.Text))
	}

	systemPrompt := `You are an expert video game analyst.
Analyze the provided reviews for the given game on the specified platform.
Separate your analysis into what critics liked/disliked, and what users liked/disliked.
Write all summary values in Russian (на русском языке), even though the reviews may be in English.
You must respond with ONLY a valid JSON object strictly matching this schema:
{
  "critic_pros": "concise summary of what critics liked",
  "critic_cons": "concise summary of what critics disliked",
  "user_pros": "concise summary of what players liked",
  "user_cons": "concise summary of what players disliked"
}`

	userPrompt := fmt.Sprintf("Game: %s\nPlatform: %s\n\nCRITIC REVIEWS:\n%s\n\nUSER REVIEWS:\n%s",
		gameTitle, platform, criticText.String(), userText.String())

	effectiveModel := c.GetOrDiscoverModel(ctx)

	payload := map[string]interface{}{
		"model": effectiveModel,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"response_format": map[string]string{"type": "json_object"},
		"temperature":     0.3,
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	resp, respBody, err := c.sendRequest(ctx, "POST", "/chat/completions", bodyBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: request failed: %v", ErrLLMUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d: %s", ErrLLMUnavailable, resp.StatusCode, truncateText(string(respBody), 120))
	}

	content := gjson.GetBytes(respBody, "choices.0.message.content").String()
	if content == "" {
		content = gjson.GetBytes(respBody, "choices.0.message.reasoning_content").String()
	}
	cleanContent := cleanJSONMarkdown(content)

	var result SummaryResult
	if err := json.Unmarshal([]byte(cleanContent), &result); err != nil {
		return nil, fmt.Errorf("%w: invalid JSON response: %v", ErrLLMUnavailable, err)
	}

	if result.CriticPros == "" && result.CriticCons == "" && result.UserPros == "" && result.UserCons == "" {
		return nil, fmt.Errorf("%w: empty summary fields", ErrLLMUnavailable)
	}

	return &result, nil
}

const defaultTranscriptMaxChars = 8000

// TranslateToRussian переводит текст (например, описание игры) на русский язык.
// При недоступности LLM возвращает ErrLLMUnavailable: выдуманный перевод недопустим.
func (c *Client) TranslateToRussian(ctx context.Context, text string) (string, error) {
	cleanText := strings.TrimSpace(text)
	if cleanText == "" {
		return "", fmt.Errorf("empty text to translate")
	}
	if c == nil || !c.HasAPIKey() {
		return "", fmt.Errorf("%w: LLM_API_KEY не задан", ErrLLMUnavailable)
	}

	systemPrompt := `Ты профессиональный переводчик игровой журналистики.
Переведи переданное описание видеоигры на русский язык.
Требования:
- Сохрани разбиение на абзацы (пустая строка между абзацами).
- Не добавляй комментарии, пояснения или кавычки - верни только перевод.
- Сохрани названия игр, студий и торговые марки без перевода.`

	effectiveModel := c.GetOrDiscoverModel(ctx)

	payload := map[string]interface{}{
		"model": effectiveModel,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": cleanText},
		},
		"temperature": 0.2,
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	resp, respBody, err := c.sendRequest(ctx, "POST", "/chat/completions", bodyBytes)
	if err != nil {
		return "", fmt.Errorf("%w: request failed: %v", ErrLLMUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: status %d: %s", ErrLLMUnavailable, resp.StatusCode, truncateText(string(respBody), 120))
	}

	content := strings.TrimSpace(gjson.GetBytes(respBody, "choices.0.message.content").String())
	if content == "" {
		content = strings.TrimSpace(gjson.GetBytes(respBody, "choices.0.message.reasoning_content").String())
	}
	if content == "" {
		return "", fmt.Errorf("%w: empty translation response", ErrLLMUnavailable)
	}

	return cleanJSONMarkdown(content), nil
}

// SummarizeVideoTranscript отправляет транскрипт рассказа блогера в LLM для формирования заключения.
func (c *Client) SummarizeVideoTranscript(ctx context.Context, gameTitle, channelName, videoTitle, transcript string) (string, error) {
	limit := defaultTranscriptMaxChars
	if c != nil && c.transcriptMaxChars != 0 {
		limit = c.transcriptMaxChars
	}
	return c.SummarizeVideoTranscriptWithLimit(ctx, gameTitle, channelName, videoTitle, transcript, limit)
}

// SummarizeVideoTranscriptWithLimit как SummarizeVideoTranscript, но с явным лимитом
// на длину транскрипта в рунах. maxChars <= 0 отключает обрезку.
func (c *Client) SummarizeVideoTranscriptWithLimit(ctx context.Context, gameTitle, channelName, videoTitle, transcript string, maxChars int) (string, error) {
	cleanTranscript := strings.TrimSpace(transcript)
	if cleanTranscript == "" {
		return "", fmt.Errorf("empty transcript")
	}

	cleanTranscript = truncateTranscript(cleanTranscript, maxChars)

	if c == nil || !c.HasAPIKey() {
		return generateFallbackTranscriptSummary(gameTitle, channelName, cleanTranscript), nil
	}

	systemPrompt := `Ты игровой журналист и аналитик.
Твоя задача — составить краткое и емкое заключение (2-4 предложения) на русском языке о том, как блогер оценил игру в своем летсплее/обзоре.
Пиши от третьего лица (например: "Блогер отмечает...", "Автор ролика обращает внимание...").
Опирайся исключительно на реальный текст транскрипта рассказа:
- Что блогеру понравилось в геймплее, графике или атмосфере?
- С какими багами, недостатками или сложностями он столкнулся?
- Каков его финальный вердикт игре?
Ответь ТОЛЬКО готовым текстом заключения, без лишних вступлений.`

	userPrompt := fmt.Sprintf("Игра: %s\nКанал: %s\nВидео: %s\n\nТранскрипт рассказа блогера:\n%s",
		gameTitle, channelName, videoTitle, cleanTranscript)

	effectiveModel := c.GetOrDiscoverModel(ctx)

	payload := map[string]interface{}{
		"model": effectiveModel,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"temperature": 0.3,
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	resp, respBody, err := c.sendRequest(ctx, "POST", "/chat/completions", bodyBytes)
	if err != nil || resp.StatusCode != http.StatusOK {
		return generateFallbackTranscriptSummary(gameTitle, channelName, cleanTranscript), nil
	}

	content := strings.TrimSpace(gjson.GetBytes(respBody, "choices.0.message.content").String())
	if content == "" {
		content = strings.TrimSpace(gjson.GetBytes(respBody, "choices.0.message.reasoning_content").String())
	}
	if content != "" {
		return content, nil
	}

	return generateFallbackTranscriptSummary(gameTitle, channelName, cleanTranscript), nil
}

func generateFallbackTranscriptSummary(gameTitle, channelName, transcript string) string {
	clean := strings.TrimSpace(transcript)
	if r := []rune(clean); len(r) > 300 {
		clean = string(r[:300]) + "..."
	}
	if channelName != "" {
		return fmt.Sprintf("Блогер (%s) проходит игру %s и комментирует происходящее: «%s».", channelName, gameTitle, clean)
	}
	return fmt.Sprintf("В летсплее по игре %s блогер отмечает: «%s».", gameTitle, clean)
}

// GetEmbedding получает векторное представление текста: через встроенный Go-векторизатор
// либо через внешний OpenAI-совместимый эндпоинт с автоматическим фолбэком.
func (c *Client) GetEmbedding(ctx context.Context, text string) ([]float32, error) {
	if c == nil || c.embeddingEngine == "local" || c.embeddingModel == "local" || c.embeddingModel == "" || c.embeddingAPIKey == "" {
		if c != nil && c.vectorizer != nil {
			return c.vectorizer.Vectorize(text), nil
		}
		return NewVectorizer(256).Vectorize(text), nil
	}

	payload := map[string]interface{}{
		"model": c.embeddingModel,
		"input": text,
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return c.vectorizer.Vectorize(text), nil
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.embeddingBaseURL+"/embeddings", bytes.NewReader(bodyBytes))
	if err != nil {
		return c.vectorizer.Vectorize(text), nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.embeddingAPIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// При сбое соединения переключаемся на локальный Go-векторизатор
		return c.vectorizer.Vectorize(text), nil
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		// При недоступности эндпоинта (например 404 у провайдера) плавный переход на Go-векторизатор
		return c.vectorizer.Vectorize(text), nil
	}

	rawArr := gjson.GetBytes(respBody, "data.0.embedding").Array()
	if len(rawArr) == 0 {
		return c.vectorizer.Vectorize(text), nil
	}

	vec := make([]float32, len(rawArr))
	for i, val := range rawArr {
		vec[i] = float32(val.Float())
	}

	return vec, nil
}

func truncateText(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// truncateTranscript обрезает текст по рунам (а не байтам), чтобы не разрезать
// многобайтовые символы. При превышении лимита сохраняет начало и конец, так как
// вердикт блогера обычно звучит в конце ролика. maxChars <= 0 отключает обрезку.
func truncateTranscript(s string, maxChars int) string {
	if maxChars <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	head := maxChars / 2
	tail := maxChars - head
	return string(runes[:head]) + " … " + string(runes[len(runes)-tail:])
}
