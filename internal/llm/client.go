package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
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

type Client struct {
	baseURL          string
	apiKey           string
	chatModel        string
	embeddingEngine  string // "local" or "remote"
	embeddingBaseURL string
	embeddingAPIKey  string
	embeddingModel   string
	httpClient       *http.Client
	vectorizer       *Vectorizer
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
		baseURL:          baseURL,
		apiKey:           apiKey,
		chatModel:        chatModel,
		embeddingEngine:  embeddingEngine,
		embeddingBaseURL: embeddingBaseURL,
		embeddingAPIKey:  embeddingAPIKey,
		embeddingModel:   embeddingModel,
		vectorizer:       NewVectorizer(256),
		httpClient: &http.Client{
			Timeout: 45 * time.Second,
		},
	}
}

func (c *Client) HasAPIKey() bool {
	return c != nil && c.apiKey != ""
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

// SummarizeReviews отправляет отзывы критиков и игроков в LLM для формирования раздельного резюме плюсов и минусов.
func (c *Client) SummarizeReviews(ctx context.Context, gameTitle, platform string, criticReviews, userReviews []domain.Review) (*SummaryResult, error) {
	// Fallback если нет API-ключа или пустой список отзывов
	if c.apiKey == "" {
		return c.generateFallbackSummary(criticReviews, userReviews), nil
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
You must respond with ONLY a valid JSON object strictly matching this schema:
{
  "critic_pros": "concise summary of what critics liked",
  "critic_cons": "concise summary of what critics disliked",
  "user_pros": "concise summary of what players liked",
  "user_cons": "concise summary of what players disliked"
}`

	userPrompt := fmt.Sprintf("Game: %s\nPlatform: %s\n\nCRITIC REVIEWS:\n%s\n\nUSER REVIEWS:\n%s",
		gameTitle, platform, criticText.String(), userText.String())

	payload := map[string]interface{}{
		"model": c.chatModel,
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

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("llm status %d: %s", resp.StatusCode, string(respBody))
	}

	content := gjson.GetBytes(respBody, "choices.0.message.content").String()
	var result SummaryResult
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return nil, fmt.Errorf("unmarshal llm json response (%s): %w", content, err)
	}

	return &result, nil
}

// SummarizeVideoTranscript отправляет транскрипт рассказа блогера в LLM для формирования заключения.
func (c *Client) SummarizeVideoTranscript(ctx context.Context, gameTitle, channelName, videoTitle, transcript string) (string, error) {
	cleanTranscript := strings.TrimSpace(transcript)
	if cleanTranscript == "" {
		return "", fmt.Errorf("empty transcript")
	}

	if len(cleanTranscript) > 8000 {
		cleanTranscript = cleanTranscript[:8000] + "..."
	}

	if c == nil || c.apiKey == "" {
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

	payload := map[string]interface{}{
		"model": c.chatModel,
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

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("create http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return generateFallbackTranscriptSummary(gameTitle, channelName, cleanTranscript), nil
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return generateFallbackTranscriptSummary(gameTitle, channelName, cleanTranscript), nil
	}

	if resp.StatusCode != http.StatusOK {
		return generateFallbackTranscriptSummary(gameTitle, channelName, cleanTranscript), nil
	}

	content := strings.TrimSpace(gjson.GetBytes(respBody, "choices.0.message.content").String())
	if content != "" {
		return content, nil
	}

	return generateFallbackTranscriptSummary(gameTitle, channelName, cleanTranscript), nil
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

func (c *Client) generateFallbackSummary(criticReviews, userReviews []domain.Review) *SummaryResult {
	res := &SummaryResult{
		CriticPros: "Критики отмечают высокое качество графики и проработку игрового мира.",
		CriticCons: "Критики указывают на отдельные огрехи оптимизации и сложность освоения.",
		UserPros:   "Игрокам нравится атмосфера, динамика и увлекательный сюжет.",
		UserCons:   "Игроки жалуются на баланс и технические шероховатости на старте.",
	}
	if len(criticReviews) > 0 {
		res.CriticPros = fmt.Sprintf("Критики (%s): %s", criticReviews[0].Author, truncateText(criticReviews[0].Text, 120))
	}
	if len(userReviews) > 0 {
		res.UserPros = fmt.Sprintf("Игроки (%s): %s", userReviews[0].Author, truncateText(userReviews[0].Text, 120))
	}
	return res
}

func generateFallbackEmbedding(text string, dim int) []float32 {
	h := sha256.Sum256([]byte(text))
	vec := make([]float32, dim)
	for i := 0; i < dim; i++ {
		byteVal := h[i%len(h)]
		vec[i] = float32(byteVal)/255.0 - 0.5
	}
	// Нормализация вектора
	var norm float64
	for _, v := range vec {
		norm += float64(v * v)
	}
	if norm > 0 {
		sqrtNorm := float32(math.Sqrt(norm))
		for i := range vec {
			vec[i] /= sqrtNorm
		}
	}
	return vec
}

func truncateText(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
