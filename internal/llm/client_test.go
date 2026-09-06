package llm_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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

