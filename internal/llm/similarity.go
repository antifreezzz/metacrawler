package llm

import (
	"math"
	"sort"

	"metacrawler/internal/domain"
)

type SimilarGame struct {
	GameID string  `json:"game_id"`
	Score  float32 `json:"score"`
}

// CosineSimilarity вычисляет косинусное сходство между двумя векторами.
func CosineSimilarity(v1, v2 []float32) float32 {
	if len(v1) == 0 || len(v2) == 0 || len(v1) != len(v2) {
		return 0
	}

	var dot, norm1, norm2 float64
	for i := range v1 {
		dot += float64(v1[i]) * float64(v2[i])
		norm1 += float64(v1[i]) * float64(v1[i])
		norm2 += float64(v2[i]) * float64(v2[i])
	}

	if norm1 == 0 || norm2 == 0 {
		return 0
	}

	sim := dot / (math.Sqrt(norm1) * math.Sqrt(norm2))
	return float32(sim)
}

// FindTopSimilar возвращает Top-K похожих игр, исключая целевую игру.
func FindTopSimilar(targetGameID string, targetVector []float32, all []domain.GameEmbedding, topK int) []SimilarGame {
	if len(targetVector) == 0 || len(all) == 0 || topK <= 0 {
		return nil
	}

	var scored []SimilarGame
	for _, item := range all {
		if item.GameID == targetGameID {
			continue
		}
		sim := CosineSimilarity(targetVector, item.Vector)
		scored = append(scored, SimilarGame{
			GameID: item.GameID,
			Score:  sim,
		})
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})

	if len(scored) > topK {
		scored = scored[:topK]
	}
	return scored
}
