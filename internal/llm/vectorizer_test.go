package llm_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"metacrawler/internal/llm"
)

func TestGoVectorizer_Basic(t *testing.T) {
	v := llm.NewVectorizer(256)
	vec := v.Vectorize("Elden Ring. An action RPG game in an open world. Developer: FromSoftware")
	require.Len(t, vec, 256)

	// Проверка L2-нормализации
	var sumSq float64
	for _, val := range vec {
		sumSq += float64(val * val)
	}
	require.InDelta(t, 1.0, sumSq, 0.01)
}

func TestGoVectorizer_Similarity(t *testing.T) {
	v := llm.NewVectorizer(256)

	eldenRing := v.Vectorize("Elden Ring. Dark fantasy action RPG by FromSoftware with open world exploration and tough bosses.")
	darkSouls := v.Vectorize("Dark Souls 3. Hardcore action RPG by FromSoftware featuring dark fantasy atmosphere and bosses.")
	fifa := v.Vectorize("EA Sports FC 24. Football sports simulation with soccer teams and career mode by EA Sports.")

	simEldenSouls := llm.CosineSimilarity(eldenRing, darkSouls)
	simEldenFifa := llm.CosineSimilarity(eldenRing, fifa)

	t.Logf("Sim(Elden Ring, Dark Souls 3): %.3f", simEldenSouls)
	t.Logf("Sim(Elden Ring, FIFA 24): %.3f", simEldenFifa)

	require.Greater(t, simEldenSouls, simEldenFifa, "Elden Ring should be much more similar to Dark Souls than to FIFA")
	require.Greater(t, simEldenSouls, float32(0.35), "Elden Ring and Dark Souls should have solid similarity")
	require.Less(t, simEldenFifa, float32(0.20), "Elden Ring and FIFA should have low similarity")
}
