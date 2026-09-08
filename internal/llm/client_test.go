package llm_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"metacrawler/internal/domain"
	"metacrawler/internal/llm"
)

func TestCosineSimilarity(t *testing.T) {
	v1 := []float32{1.0, 0.0, 0.0}
	v2 := []float32{1.0, 0.0, 0.0}
	sim := llm.CosineSimilarity(v1, v2)
	require.InDelta(t, 1.0, sim, 0.0001)

	vOrthogonal := []float32{0.0, 1.0, 0.0}
	simOrth := llm.CosineSimilarity(v1, vOrthogonal)
	require.InDelta(t, 0.0, simOrth, 0.0001)

	vOpposite := []float32{-1.0, 0.0, 0.0}
	simOpp := llm.CosineSimilarity(v1, vOpposite)
	require.InDelta(t, -1.0, simOpp, 0.0001)

	// Пустые или разной длины
	require.Equal(t, float32(0), llm.CosineSimilarity(nil, v1))
	require.Equal(t, float32(0), llm.CosineSimilarity(v1, []float32{1.0}))
}

func TestFindTopSimilar(t *testing.T) {
	target := domain.GameEmbedding{
		GameID: "target-game",
		Vector: []float32{1.0, 0.0, 0.0},
	}

	all := []domain.GameEmbedding{
		{GameID: "target-game", Vector: []float32{1.0, 0.0, 0.0}},
		{GameID: "game-close", Vector: []float32{0.9, 0.1, 0.0}},
		{GameID: "game-far", Vector: []float32{0.1, 0.9, 0.0}},
		{GameID: "game-opposite", Vector: []float32{-1.0, 0.0, 0.0}},
	}

	top := llm.FindTopSimilar(target.GameID, target.Vector, all, 2)
	require.Len(t, top, 2)
	require.Equal(t, "game-close", top[0].GameID)
	require.Equal(t, "game-far", top[1].GameID)
}

func TestSummarizeReviews_Mock(t *testing.T) {
	expectedSummary := llm.SummaryResult{
		CriticPros: "Outstanding visuals and deep combat mechanics.",
		CriticCons: "High difficulty spike in early game.",
		UserPros:   "Huge replayability and incredible open world.",
		UserCons:   "Occasional frame rate drops on performance mode.",
	}

	rawJSON, err := json.Marshal(expectedSummary)
	require.NoError(t, err)

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		resp := map[string]interface{}{
			"choices": []map[string]interface{}{
				{
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": string(rawJSON),
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	client := llm.NewClient(mockServer.URL+"/v1", "test-key", "gpt-4o-mini", "text-embedding-3-small")

	criticReviews := []domain.Review{
		{ReviewType: domain.ReviewTypeCritic, Author: "IGN", Text: "Amazing game with great combat."},
	}
	userReviews := []domain.Review{
		{ReviewType: domain.ReviewTypeUser, Author: "Player1", Text: "Love the world and exploration!"},
	}

	res, err := client.SummarizeReviews(context.Background(), "Elden Ring", "ps5", criticReviews, userReviews)
	require.NoError(t, err)
	require.Equal(t, expectedSummary.CriticPros, res.CriticPros)
	require.Equal(t, expectedSummary.CriticCons, res.CriticCons)
	require.Equal(t, expectedSummary.UserPros, res.UserPros)
	require.Equal(t, expectedSummary.UserCons, res.UserCons)
}

func TestSummarizeReviews_RespectsConfiguredTimeout(t *testing.T) {
	slowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer slowServer.Close()

	client := llm.NewClient(slowServer.URL+"/v1", "test-key", "gpt-4o", "local")
	client.SetTimeout(200 * time.Millisecond)

	start := time.Now()
	_, err := client.SummarizeReviews(context.Background(), "Game", "pc",
		[]domain.Review{{ReviewType: domain.ReviewTypeCritic, Author: "IGN", Text: "Good."}},
		[]domain.Review{{ReviewType: domain.ReviewTypeUser, Author: "P", Text: "Fun."}})
	elapsed := time.Since(start)

	require.Error(t, err)
	require.ErrorIs(t, err, llm.ErrLLMUnavailable)
	require.Less(t, elapsed, 1500*time.Millisecond,
		"таймаут должен применяться из конфигурации, а не дефолтные 45с (elapsed=%v)", elapsed)
}

func TestGetEmbedding_Mock(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/embeddings", r.URL.Path)

		resp := map[string]interface{}{
			"data": []map[string]interface{}{
				{
					"embedding": []float32{0.1, 0.2, 0.3},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	client := llm.NewClient(mockServer.URL+"/v1", "test-key", "gpt-4o-mini", "text-embedding-3-small")

	vec, err := client.GetEmbedding(context.Background(), "A fantasy action RPG.")
	require.NoError(t, err)
	require.Equal(t, []float32{0.1, 0.2, 0.3}, vec)
}

func TestGetEmbedding_Remote404Fallback(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<!DOCTYPE html><html><body>Not Found</body></html>"))
	}))
	defer mockServer.Close()

	client := llm.NewClient(mockServer.URL+"/v1", "test-key", "gpt-4o-mini", "text-embedding-3-small")

	vec, err := client.GetEmbedding(context.Background(), "A fantasy action RPG.")
	require.NoError(t, err)
	require.Len(t, vec, 256, "Should gracefully fall back to Go vectorizer on 404")
}

func TestLlamaCpp_DynamicModelAndNoAPIKey(t *testing.T) {
	requestedModel := ""
	mockLlama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			// llama.cpp /models format
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]interface{}{
					{"id": "/models/LiquidAI-LFM2.5-Q4_K_M.gguf"},
				},
			})
		case "/v1/chat/completions":
			var req map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if m, ok := req["model"].(string); ok {
				requestedModel = m
			}
			// Simulate markdown wrapped JSON and reasoning
			resp := map[string]interface{}{
				"choices": []map[string]interface{}{
					{
						"message": map[string]interface{}{
							"role":              "assistant",
							"content":           "```json\n{\"critic_pros\": \"Great!\", \"critic_cons\": \"None\", \"user_pros\": \"Cool\", \"user_cons\": \"None\"}\n```",
							"reasoning_content": "Thinking about the game...",
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockLlama.Close()

	// Empty apiKey and model="auto" on local endpoint
	client := llm.NewClient(mockLlama.URL+"/v1", "", "auto", "local")
	require.True(t, client.HasAPIKey(), "Local endpoint should be treated as active even without API key")

	criticReviews := []domain.Review{{ReviewType: domain.ReviewTypeCritic, Author: "IGN", Text: "Awesome"}}
	userReviews := []domain.Review{{ReviewType: domain.ReviewTypeUser, Author: "User1", Text: "Fun"}}

	summary, err := client.SummarizeReviews(context.Background(), "Game X", "pc", criticReviews, userReviews)
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.Equal(t, "Great!", summary.CriticPros)
	require.Equal(t, "/models/LiquidAI-LFM2.5-Q4_K_M.gguf", requestedModel, "Should dynamically discover the loaded model from /models")
}

func TestSummarizeReviews_NoAPIKey_ReturnsUnavailableError(t *testing.T) {
	// Честность данных: без ключа и без локального эндпоинта резюме не выдумывается.
	client := llm.NewClient("http://llm.example.com/v1", "", "gpt-4o-mini", "local")
	require.False(t, client.HasAPIKey())

	criticReviews := []domain.Review{
		{ReviewType: domain.ReviewTypeCritic, Author: "IGN", Text: "Great gameplay and solid graphics."},
	}
	userReviews := []domain.Review{
		{ReviewType: domain.ReviewTypeUser, Author: "Player", Text: "Enjoyed every hour of it."},
	}

	res, err := client.SummarizeReviews(context.Background(), "Test Game", "pc", criticReviews, userReviews)
	require.ErrorIs(t, err, llm.ErrLLMUnavailable)
	require.Nil(t, res)
}

func TestSummarizeReviews_ServerError_ReturnsError(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "GPU out of memory", http.StatusInternalServerError)
	}))
	defer mockServer.Close()

	client := llm.NewClient(mockServer.URL+"/v1", "test-key", "gpt-4o", "local")

	criticReviews := []domain.Review{
		{ReviewType: domain.ReviewTypeCritic, Author: "GameSpot", Text: "Superb combat system and world building."},
	}
	userReviews := []domain.Review{
		{ReviewType: domain.ReviewTypeUser, Author: "Gamer99", Text: "One of the best games ever made."},
	}

	res, err := client.SummarizeReviews(context.Background(), "Test Game", "pc", criticReviews, userReviews)
	require.Error(t, err, "ошибка LLM должна возвращаться, а не маскироваться выдуманным текстом")
	require.Nil(t, res)
}

func TestSummarizeReviews_InvalidJSON_ReturnsError(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"choices": []map[string]interface{}{
				{
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": "I apologize, but as an AI language model I cannot format this.",
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	client := llm.NewClient(mockServer.URL+"/v1", "test-key", "gpt-4o", "local")

	criticReviews := []domain.Review{
		{ReviewType: domain.ReviewTypeCritic, Author: "IGN", Text: "Great gameplay and solid graphics."},
	}
	userReviews := []domain.Review{
		{ReviewType: domain.ReviewTypeUser, Author: "Player", Text: "Enjoyed every hour of it."},
	}

	res, err := client.SummarizeReviews(context.Background(), "Test Game", "pc", criticReviews, userReviews)
	require.Error(t, err, "нераспарсимый ответ LLM должен возвращаться ошибкой, без выдуманного резюме")
	require.Nil(t, res)
}
