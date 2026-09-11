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

// withUserScore подменяет значение userscore в фикстуре страницы user-reviews.
func withUserScore(html []byte, score string) []byte {
	s := string(html)
	s = strings.ReplaceAll(s, "User score 8.4 out", "User score "+score+" out")
	s = strings.ReplaceAll(s, ">8.4</span>", ">"+score+"</span>")
	return []byte(s)
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
			score := platformUserScores[r.URL.Query().Get("platform")]
			if score == "" {
				score = "8.4"
			}
			_, _ = w.Write(withUserScore(userHTML, score))
		default:
			_, _ = w.Write(mainHTML)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &fetched
}

func TestFetchGameDetails_FetchesPerPlatformSubpages(t *testing.T) {
	srv, fetched := newFixtureServer(t, http.StatusOK, http.StatusOK)

	client, err := scraper.NewClientWithBaseURL(srv.URL)
	require.NoError(t, err)

	game, reviews, err := client.FetchGameDetails(context.Background(), "elden-ring")
	require.NoError(t, err)
	require.NotNil(t, game)
	require.Equal(t, "Elden Ring", game.Title)

	// Главная страница и по паре подстраниц отзывов на каждую платформу
	require.Equal(t, 1, (*fetched)["/game/elden-ring/"])
	require.NotEmpty(t, game.Platforms)
	for _, p := range game.Platforms {
		require.Equal(t, 1, (*fetched)["/game/elden-ring/critic-reviews/?platform="+p.Platform], "critic page for %s", p.Platform)
		require.Equal(t, 1, (*fetched)["/game/elden-ring/user-reviews/?platform="+p.Platform], "user page for %s", p.Platform)
	}

	// Каждый отзыв имеет установленную платформу (строгая привязка).
	require.NotEmpty(t, reviews)
	perPlatform := map[string]int{}
	for _, r := range reviews {
		require.NotEmpty(t, r.Platform, "review without platform must not be collected: %+v", r)
		perPlatform[r.Platform]++
	}
	for _, p := range game.Platforms {
		require.Greater(t, perPlatform[p.Platform], 0, "platform %s must have reviews", p.Platform)
	}
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
	// Ошибка подстраниц не должна валить сбор целиком
	require.NoError(t, err)
	require.NotNil(t, game)
	require.Len(t, reviews, 14) // только отзывы с главной страницы
	for _, p := range game.Platforms {
		require.Nil(t, p.Userscore, "при сбое подстраницы userscore не выдумывается")
	}
}
