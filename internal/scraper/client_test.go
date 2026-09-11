package scraper_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"metacrawler/internal/scraper"
)

// platformUserScores задаёт разные userscore для платформ, чтобы тест
// доказывал платформенную привязку, а не копирование одного значения.
var platformUserScores = map[string]string{
	"pc":            "7.1",
	"playstation-4": "8.0",
	"xbox-series-x": "8.2",
	"playstation-5": "8.4",
}

func userScorePage(score string) string {
	return fmt.Sprintf(`<!DOCTYPE html><html><body>
<div class="product-reviews-score" data-testid="score-card-overview">
  <div class="c-siteReviewScore" title="User score %s out of 10" aria-label="User score %s out of 10"><span>%s</span></div>
</div></body></html>`, score, score, score)
}

func newFixtureServer(t *testing.T, criticStatus, userStatus int) (*httptest.Server, *map[string]int) {
	t.Helper()

	mainHTML, err := os.ReadFile("../../testdata/game_details.html")
	require.NoError(t, err)
	criticHTML, err := os.ReadFile("../../testdata/game_critic_reviews.html")
	require.NoError(t, err)
	userHTML, err := os.ReadFile("../../testdata/game_user_reviews.html")
	require.NoError(t, err)

	var mu sync.Mutex
	fetched := map[string]int{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fetched[r.URL.RequestURI()]++
		mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/critic-reviews/"):
			if criticStatus != http.StatusOK {
				w.WriteHeader(criticStatus)
				return
			}
			_, _ = w.Write(criticHTML)
		case strings.HasSuffix(r.URL.Path, "/user-reviews/"):
			if userStatus != http.StatusOK {
				w.WriteHeader(userStatus)
				return
			}
			if platform := r.URL.Query().Get("platform"); platform != "" {
				if score, ok := platformUserScores[platform]; ok {
					_, _ = w.Write([]byte(userScorePage(score)))
					return
				}
			}
			_, _ = w.Write(userHTML)
		default:
			_, _ = w.Write(mainHTML)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &fetched
}

func TestFetchGameDetails_FetchesReviewSubpages(t *testing.T) {
	srv, fetched := newFixtureServer(t, http.StatusOK, http.StatusOK)

	client, err := scraper.NewClientWithBaseURL(srv.URL)
	require.NoError(t, err)

	game, reviews, err := client.FetchGameDetails(context.Background(), "elden-ring")
	require.NoError(t, err)
	require.NotNil(t, game)
	require.Equal(t, "Elden Ring", game.Title)

	// Главная страница, две подстраницы отзывов и по одному запросу
	// userscore на каждую платформу
	require.Equal(t, 1, (*fetched)["/game/elden-ring/"])
	require.Equal(t, 1, (*fetched)["/game/elden-ring/critic-reviews/"])
	require.Equal(t, 1, (*fetched)["/game/elden-ring/user-reviews/"])
	for platform := range platformUserScores {
		require.Equal(t, 1, (*fetched)["/game/elden-ring/user-reviews/?platform="+platform])
	}

	// Отзывы объединены со всех трех страниц: 14 (главная) + 10 (critic) + 50 (user)
	require.Len(t, reviews, 74)

	// Платформа атрибутирована во всех отзывах (фикстуры - страница PS5)
	var ps5Count, noPlatform int
	for _, r := range reviews {
		switch r.Platform {
		case "playstation-5":
			ps5Count++
		case "":
			noPlatform++
		}
	}
	require.Equal(t, 74, ps5Count)
	require.Equal(t, 0, noPlatform)
}

// TestFetchGameDetails_PerPlatformUserScores проверяет, что userscore каждой
// платформы берется из её собственной страницы, а не копируется со страницы игры.
func TestFetchGameDetails_PerPlatformUserScores(t *testing.T) {
	srv, _ := newFixtureServer(t, http.StatusOK, http.StatusOK)

	client, err := scraper.NewClientWithBaseURL(srv.URL)
	require.NoError(t, err)

	game, _, err := client.FetchGameDetails(context.Background(), "elden-ring")
	require.NoError(t, err)
	require.NotEmpty(t, game.Platforms)

	for _, p := range game.Platforms {
		want, ok := platformUserScores[p.Platform]
		require.True(t, ok, "unexpected platform %q", p.Platform)
		require.NotNil(t, p.Userscore, "platform %q must have its own userscore", p.Platform)
		require.Equal(t, want, fmt.Sprintf("%.1f", *p.Userscore))
	}
}

func TestFetchGameDetails_SubpageFailureIsNotFatal(t *testing.T) {
	srv, _ := newFixtureServer(t, http.StatusNotFound, http.StatusInternalServerError)

	client, err := scraper.NewClientWithBaseURL(srv.URL)
	require.NoError(t, err)

	game, reviews, err := client.FetchGameDetails(context.Background(), "elden-ring")
	// Ошибка одной подстраницы не должна валить сбор целиком
	require.NoError(t, err)
	require.NotNil(t, game)
	require.Len(t, reviews, 14) // только отзывы с главной страницы
	for _, p := range game.Platforms {
		require.Nil(t, p.Userscore, "при сбое подстраницы userscore не выдумывается")
	}
}
