package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"metacrawler/internal/domain"
	"metacrawler/internal/storage"
)

func newTestDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = db.Close()
	})
	return db
}

func TestMigrate(t *testing.T) {
	db := newTestDB(t)
	require.NotNil(t, db)
}

func TestGameRepository_UpsertWithPlatforms(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	metaScore90 := 90
	userScore85 := 8.5

	game := &domain.Game{
		ID:          "game-1",
		Slug:        "elden-ring",
		Title:       "Elden Ring",
		CoverURL:    "https://example.com/cover.jpg",
		Developer:   "FromSoftware",
		Description: "A fantasy action-RPG adventure.",
		VideoURL:    "https://example.com/trailer.mp4",
		Platforms: []domain.GamePlatform{
			{
				Platform:    "pc",
				Metascore:   &metaScore90,
				Userscore:   &userScore85,
				PlatformURL: "https://www.metacritic.com/game/elden-ring/pc",
			},
			{
				Platform:    "ps5",
				Metascore:   &metaScore90,
				Userscore:   &userScore85,
				PlatformURL: "https://www.metacritic.com/game/elden-ring/ps5",
			},
		},
	}

	err := db.UpsertGame(ctx, game)
	require.NoError(t, err)

	fetched, err := db.GetGameBySlug(ctx, "elden-ring")
	require.NoError(t, err)
	require.Equal(t, "Elden Ring", fetched.Title)
	require.Len(t, fetched.Platforms, 2)

	newMetaScore96 := 96
	game.Platforms[1].Metascore = &newMetaScore96
	err = db.UpsertGame(ctx, game)
	require.NoError(t, err)

	updated, err := db.GetGameBySlug(ctx, "elden-ring")
	require.NoError(t, err)
	require.Len(t, updated.Platforms, 2)

	var ps5Platform *domain.GamePlatform
	for i := range updated.Platforms {
		if updated.Platforms[i].Platform == "ps5" {
			ps5Platform = &updated.Platforms[i]
			break
		}
	}
	require.NotNil(t, ps5Platform)
	require.NotNil(t, ps5Platform.Metascore)
	require.Equal(t, 96, *ps5Platform.Metascore)
}

func TestReviews_DeduplicationAndTypes(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	game := &domain.Game{
		ID:    "game-1",
		Slug:  "zelda-totk",
		Title: "The Legend of Zelda: Tears of the Kingdom",
		Platforms: []domain.GamePlatform{
			{Platform: "nintendo-switch", PlatformURL: "https://example.com"},
		},
	}
	err := db.UpsertGame(ctx, game)
	require.NoError(t, err)

	fetched, err := db.GetGameBySlug(ctx, "zelda-totk")
	require.NoError(t, err)
	require.Len(t, fetched.Platforms, 1)
	platformID := fetched.Platforms[0].ID

	score10 := 10.0
	criticHash := domain.ComputeContentHash(domain.ReviewTypeCritic, "IGN", "Masterpiece gameplay.")
	userHash := domain.ComputeContentHash(domain.ReviewTypeUser, "Gamer123", "Best game ever.")

	reviews := []domain.Review{
		{
			GamePlatformID: platformID,
			ReviewType:     domain.ReviewTypeCritic,
			Author:         "IGN",
			Score:          &score10,
			Text:           "Masterpiece gameplay.",
			ContentHash:    criticHash,
			DateStr:        "2023-05-12",
		},
		{
			GamePlatformID: platformID,
			ReviewType:     domain.ReviewTypeUser,
			Author:         "Gamer123",
			Score:          &score10,
			Text:           "Best game ever.",
			ContentHash:    userHash,
			DateStr:        "2023-05-13",
		},
	}

	savedCount, err := db.SaveReviews(ctx, reviews)
	require.NoError(t, err)
	require.Equal(t, 2, savedCount)

	savedCount2, err := db.SaveReviews(ctx, reviews)
	require.NoError(t, err)
	require.Equal(t, 0, savedCount2)

	fetchedReviews, err := db.GetReviewsByPlatformID(ctx, platformID)
	require.NoError(t, err)
	require.Len(t, fetchedReviews, 2)
}

func TestReviewsSummary_Upsert(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	game := &domain.Game{
		ID:    "game-1",
		Slug:  "cyberpunk-2077",
		Title: "Cyberpunk 2077",
		Platforms: []domain.GamePlatform{
			{Platform: "pc", PlatformURL: "https://example.com"},
		},
	}
	err := db.UpsertGame(ctx, game)
	require.NoError(t, err)

	fetched, err := db.GetGameBySlug(ctx, "cyberpunk-2077")
	require.NoError(t, err)
	platformID := fetched.Platforms[0].ID

	summary := &domain.PlatformSummary{
		GamePlatformID: platformID,
		CriticPros:     "Great visuals, immersive world",
		CriticCons:     "Minor bugs at launch",
		UserPros:       "Engaging story and Keanu Reeves",
		UserCons:       "Performance drops on low-end systems",
	}

	err = db.UpsertPlatformSummary(ctx, summary)
	require.NoError(t, err)

	fetchedSummary, err := db.GetPlatformSummary(ctx, platformID)
	require.NoError(t, err)
	require.NotNil(t, fetchedSummary)
	require.Equal(t, "Great visuals, immersive world", fetchedSummary.CriticPros)
}

func TestCrawlHistory_IsProcessedToday(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	today := time.Now().Format("2006-01-02")
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")

	processed, err := db.IsProcessedOnDate(ctx, "game-abc", today)
	require.NoError(t, err)
	require.False(t, processed)

	err = db.MarkProcessed(ctx, "game-abc", yesterday)
	require.NoError(t, err)

	processed, err = db.IsProcessedOnDate(ctx, "game-abc", today)
	require.NoError(t, err)
	require.False(t, processed)

	err = db.MarkProcessed(ctx, "game-abc", today)
	require.NoError(t, err)

	processed, err = db.IsProcessedOnDate(ctx, "game-abc", today)
	require.NoError(t, err)
	require.True(t, processed)
}

func TestCrawlerState(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	val, err := db.GetState(ctx, "current_page")
	require.NoError(t, err)
	require.Equal(t, "", val)

	err = db.SetState(ctx, "current_page", "5")
	require.NoError(t, err)

	val, err = db.GetState(ctx, "current_page")
	require.NoError(t, err)
	require.Equal(t, "5", val)
}

func TestGameEmbeddings_SaveAndGetAll(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	game := &domain.Game{
		ID:    "game-1",
		Slug:  "game-1",
		Title: "Game One",
	}
	err := db.UpsertGame(ctx, game)
	require.NoError(t, err)

	embedding := &domain.GameEmbedding{
		GameID:     "game-1",
		Vector:     []float32{0.1, 0.2, 0.3, 0.4},
		Dimensions: 4,
	}

	err = db.SaveEmbedding(ctx, embedding)
	require.NoError(t, err)

	all, err := db.GetAllEmbeddings(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, "game-1", all[0].GameID)
}

func TestListGames_FilterSearchSort(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	score80 := 80
	score95 := 95

	_ = db.UpsertGame(ctx, &domain.Game{
		ID: "g-1", Slug: "g-1", Title: "Cyberpunk 2077",
		Platforms: []domain.GamePlatform{{Platform: "pc", Metascore: &score80}},
	})
	_ = db.UpsertGame(ctx, &domain.Game{
		ID: "g-2", Slug: "g-2", Title: "Elden Ring",
		Platforms: []domain.GamePlatform{{Platform: "ps5", Metascore: &score95}},
	})

	// 1. Поиск по подстроке
	games, err := db.ListGames(ctx, storage.ListFilter{Search: "Elden"})
	require.NoError(t, err)
	require.Len(t, games, 1)
	require.Equal(t, "Elden Ring", games[0].Title)

	// 2. Фильтр по платформе
	games, err = db.ListGames(ctx, storage.ListFilter{Platform: "pc"})
	require.NoError(t, err)
	require.Len(t, games, 1)
	require.Equal(t, "Cyberpunk 2077", games[0].Title)

	// 3. Сортировка по Metascore убывание
	games, err = db.ListGames(ctx, storage.ListFilter{Sort: "metascore_desc"})
	require.NoError(t, err)
	require.Len(t, games, 2)
	require.Equal(t, "Elden Ring", games[0].Title) // 95 идет первым
}

func TestYouTubeAnalysis_UpsertAndGet(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	game := &domain.Game{
		ID:    "g-yt-1",
		Slug:  "elden-ring",
		Title: "Elden Ring",
	}
	require.NoError(t, db.UpsertGame(ctx, game))

	analysis := &domain.YouTubeAnalysis{
		GameID:      game.ID,
		VideoID:     "abc123xyz",
		VideoTitle:  "Elden Ring Full Walkthrough Part 1",
		VideoURL:    "https://www.youtube.com/watch?v=abc123xyz",
		ChannelName: "ProGamer",
		ViewCount:   1500000,
		Summary:     "Блоггер в восторге от масштаба мира и свободы исследования, однако отмечает высокую сложность начальных боссов.",
	}

	err := db.UpsertYouTubeAnalysis(ctx, analysis)
	require.NoError(t, err)

	fetched, err := db.GetYouTubeAnalysis(ctx, game.ID)
	require.NoError(t, err)
	require.NotNil(t, fetched)
	require.Equal(t, "abc123xyz", fetched.VideoID)
	require.Equal(t, "ProGamer", fetched.ChannelName)
	require.Equal(t, int64(1500000), fetched.ViewCount)
	require.Contains(t, fetched.Summary, "Блоггер в восторге")
}
