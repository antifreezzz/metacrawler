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
	baseURL        string
	apiKey         string
	chatModel      string
	embeddingModel string
	httpClient     *http.Client
}

func NewClient(baseURL, apiKey, chatModel, embeddingModel string) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(baseURL, "/v1") {
		baseURL += "/v1"
	}

	return &Client{
		baseURL:        baseURL,
		apiKey:         apiKey,
		chatModel:      chatModel,
		embeddingModel: embeddingModel,
		httpClient: &http.Client{
			Timeout: 45 * time.Second,
		},
	}
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

// GetEmbedding получает векторное представление текста через OpenAI-совместимый API.
func (c *Client) GetEmbedding(ctx context.Context, text string) ([]float32, error) {
	if c.apiKey == "" {
		return generateFallbackEmbedding(text, 128), nil
	}

	payload := map[string]interface{}{
		"model": c.embeddingModel,
		"input": text,
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/embeddings", bytes.NewReader(bodyBytes))
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
		return nil, fmt.Errorf("embedding status %d: %s", resp.StatusCode, string(respBody))
	}

	rawArr := gjson.GetBytes(respBody, "data.0.embedding").Array()
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
