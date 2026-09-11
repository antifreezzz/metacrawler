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

	// Проверка платформ: парсер не назначает userscore одной платформы всем.
	require.NotEmpty(t, game.Platforms)
	var hasPC, hasPS5, hasXbox bool
	for _, p := range game.Platforms {
		switch p.Platform {
		case "pc":
			hasPC = true
			require.NotNil(t, p.Metascore)
			require.Equal(t, 94, *p.Metascore)
		case "playstation-5":
			hasPS5 = true
			require.NotNil(t, p.Metascore)
			require.Equal(t, 96, *p.Metascore)
		case "xbox-series-x":
			hasXbox = true
			require.NotNil(t, p.Metascore)
			require.Equal(t, 96, *p.Metascore)
		}
		require.Nil(t, p.Userscore, "userscore должен заполняться в контексте конкретной платформы, а не копироваться со страницы")
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

func TestParseReviewSubpage_CriticReviews(t *testing.T) {
	data, err := os.ReadFile("../../testdata/game_critic_reviews.html")
	require.NoError(t, err)

	reviews := scraper.ParseReviewSubpage(data, domain.ReviewTypeCritic)
	require.Len(t, reviews, 10)

	platforms := make([]string, 0, len(reviews))
	for _, r := range reviews {
		require.Equal(t, domain.ReviewTypeCritic, r.ReviewType)
		require.NotEmpty(t, r.Author)
		require.NotEmpty(t, r.Text)
		require.NotEmpty(t, r.DateStr)
		platforms = append(platforms, r.Platform)
	}
	require.Contains(t, platforms, "playstation-5")
	require.NotContains(t, platforms, "", "платформа должна распарситься в каждой карточке")
}

func TestParseReviewSubpage_UserReviews(t *testing.T) {
	data, err := os.ReadFile("../../testdata/game_user_reviews.html")
	require.NoError(t, err)

	reviews := scraper.ParseReviewSubpage(data, domain.ReviewTypeUser)
	require.Len(t, reviews, 50)
	for _, r := range reviews {
		require.Equal(t, domain.ReviewTypeUser, r.ReviewType)
		require.NotEmpty(t, r.Author)
		require.NotEmpty(t, r.Text)
	}
}

func TestParseReviewSubpage_CleanAuthor(t *testing.T) {
	// Автор не должен содержать оценку из круга слева от имени ("100 Areajugones" - баг)
	data, err := os.ReadFile("../../testdata/game_critic_reviews.html")
	require.NoError(t, err)

	reviews := scraper.ParseReviewSubpage(data, domain.ReviewTypeCritic)
	require.NotEmpty(t, reviews)
	for _, r := range reviews {
		require.NotRegexp(t, `^\d+\s`, r.Author, "author polluted with score: %q", r.Author)
	}
}

func TestParseGameDetails_ReviewPlatform(t *testing.T) {
	data, err := os.ReadFile("../../testdata/game_details.html")
	require.NoError(t, err)

	_, reviews, err := scraper.ParseGameDetails("elden-ring", data)
	require.NoError(t, err)
	require.NotEmpty(t, reviews)
	for _, r := range reviews {
		require.Equal(t, "playstation-5", r.Platform, "review platform should be extracted from card")
	}
}

func TestParseUserScore(t *testing.T) {
	data, err := os.ReadFile("../../testdata/game_user_reviews.html")
	require.NoError(t, err)

	score := scraper.ParseUserScore(data)
	require.NotNil(t, score)
	require.InDelta(t, 8.4, *score, 0.01)
}

func TestParseUserScore_TBD(t *testing.T) {
	data := []byte(`<div class="product-reviews-score" data-testid="score-card-overview"><div class="c-siteReviewScore" title="User score tbd out of 10" aria-label="User score tbd out of 10"><span>tbd</span></div></div>`)
	require.Nil(t, scraper.ParseUserScore(data))
}

func TestParseUserScore_Empty(t *testing.T) {
	require.Nil(t, scraper.ParseUserScore([]byte(`<html><body><p>no scores here</p></body></html>`)))
}

func TestParseGameDetails_UnescapesHTMLEntities(t *testing.T) {
	// Metacritic кладёт в JSON-LD сырые HTML-сущности (&quot; &bull; &amp;),
	// т.к. блок живёт внутри <script> и HTML-парсер его не декодирует.
	// Заодно схлопываем многократные пробелы, переносы абзацев сохраняем.
	data := []byte(`<!DOCTYPE html><html><head>
<script type="application/ld+json">{"@type":"VideoGame","name":"Fun Puzzle","description":"A game  &quot;Move the jewels&quot; &bull; features &amp;   modes.\n\nSecond  paragraph.","image":"https://example.com/cover.jpg","datePublished":"2026-09-08"}</script>
</head><body></body></html>`)

	game, _, err := scraper.ParseGameDetails("fun-puzzle", data)
	require.NoError(t, err)
	require.Equal(t, "A game \"Move the jewels\" • features & modes.\n\nSecond paragraph.", game.Description)
}

func TestNormalizeReleaseDate(t *testing.T) {
	require.Equal(t, "2022-02-25", scraper.NormalizeReleaseDate("2022-02-25"))
	require.Equal(t, "2022-02-25", scraper.NormalizeReleaseDate("2022-02-25T00:00:00.000Z"))
	require.Equal(t, "2022-02-25", scraper.NormalizeReleaseDate("Feb 25, 2022"))
	require.Equal(t, "2022-02-25", scraper.NormalizeReleaseDate("February 25, 2022"))
	require.Equal(t, "2024-01-05", scraper.NormalizeReleaseDate("5 Jan 2024"))
	require.Equal(t, "", scraper.NormalizeReleaseDate(""))
}
