package scraper_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"metacrawler/internal/domain"
	"metacrawler/internal/scraper"
)

func TestParseNewReleasesList(t *testing.T) {
	data, err := os.ReadFile("../../testdata/new_releases.html")
	require.NoError(t, err)

	slugs, err := scraper.ParseNewReleases(data)
	require.NoError(t, err)
	require.NotEmpty(t, slugs)
	// Должно быть 20 игр из раздела New Releases
	require.Equal(t, 20, len(slugs))
	require.Equal(t, "onimusha-way-of-the-sword", slugs[0])
}

func TestParseBrowseCatalogList(t *testing.T) {
	data, err := os.ReadFile("../../testdata/browse_new.html")
	require.NoError(t, err)

	slugs, err := scraper.ParseBrowseCatalog(data)
	require.NoError(t, err)
	require.NotEmpty(t, slugs)
	require.GreaterOrEqual(t, len(slugs), 20)
	require.Equal(t, "water-margin-heroes-eight-acts-of-battle", slugs[0])
}

func TestParseGameDetails(t *testing.T) {
	data, err := os.ReadFile("../../testdata/game_details.html")
	require.NoError(t, err)

	game, reviews, err := scraper.ParseGameDetails("elden-ring", data)
	require.NoError(t, err)
	require.NotNil(t, game)
	require.Equal(t, "elden-ring", game.Slug)
	require.Equal(t, "Elden Ring", game.Title)
	require.Contains(t, game.Developer, "From")
	require.NotEmpty(t, game.CoverURL)
	require.NotEmpty(t, game.Description)
	require.NotEmpty(t, game.VideoURL)
	require.Equal(t, "2022-02-25", game.ReleaseDate)

	// Проверка платформ
	require.NotEmpty(t, game.Platforms)
	var hasPC, hasPS5, hasXbox bool
	for _, p := range game.Platforms {
		switch p.Platform {
		case "pc":
			hasPC = true
			require.NotNil(t, p.Metascore)
			require.Equal(t, 94, *p.Metascore)
			require.NotNil(t, p.Userscore)
			require.InDelta(t, 8.4, *p.Userscore, 0.01)
		case "playstation-5":
			hasPS5 = true
			require.NotNil(t, p.Metascore)
			require.Equal(t, 96, *p.Metascore)
			require.NotNil(t, p.Userscore)
			require.InDelta(t, 8.4, *p.Userscore, 0.01)
		case "xbox-series-x":
			hasXbox = true
			require.NotNil(t, p.Metascore)
			require.Equal(t, 96, *p.Metascore)
			require.NotNil(t, p.Userscore)
			require.InDelta(t, 8.4, *p.Userscore, 0.01)
		}
	}
	require.True(t, hasPC, "should have PC platform")
	require.True(t, hasPS5, "should have PS5 platform")
	require.True(t, hasXbox, "should have Xbox platform")

	// Проверка отзывов (критики и игроки)
	require.NotEmpty(t, reviews)
	var hasCritic, hasUser bool
	for _, r := range reviews {
		if r.ReviewType == domain.ReviewTypeCritic {
			hasCritic = true
		}
		if r.ReviewType == domain.ReviewTypeUser {
			hasUser = true
			require.NotEmpty(t, r.Author)
			require.NotEmpty(t, r.Text)
		}
	}
	require.True(t, hasCritic || hasUser, "should have parsed reviews")
}

func TestNormalizeReleaseDate(t *testing.T) {
	require.Equal(t, "2022-02-25", scraper.NormalizeReleaseDate("2022-02-25"))
	require.Equal(t, "2022-02-25", scraper.NormalizeReleaseDate("2022-02-25T00:00:00.000Z"))
	require.Equal(t, "2022-02-25", scraper.NormalizeReleaseDate("Feb 25, 2022"))
	require.Equal(t, "2022-02-25", scraper.NormalizeReleaseDate("February 25, 2022"))
	require.Equal(t, "2024-01-05", scraper.NormalizeReleaseDate("5 Jan 2024"))
	require.Equal(t, "", scraper.NormalizeReleaseDate(""))
}
