package storage_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "modernc.org/sqlite"

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

func TestReviews_PlatformStoredAndReturned(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	game := &domain.Game{
		Slug:  "platform-reviews",
		Title: "Platform Reviews Game",
		Platforms: []domain.GamePlatform{
			{Platform: "pc"},
		},
	}
	require.NoError(t, db.UpsertGame(ctx, game))

	fetched, err := db.GetGameBySlug(ctx, "platform-reviews")
	require.NoError(t, err)
	require.Len(t, fetched.Platforms, 1)
	platformID := fetched.Platforms[0].ID

	reviews := []domain.Review{
		{GamePlatformID: platformID, ReviewType: domain.ReviewTypeCritic, Author: "IGN", Text: "Great on PC.", Platform: "pc"},
		{GamePlatformID: platformID, ReviewType: domain.ReviewTypeUser, Author: "User1", Text: "Runs well.", Platform: ""},
	}
	savedCount, err := db.SaveReviews(ctx, reviews)
	require.NoError(t, err)
	require.Equal(t, 2, savedCount)

	fetchedReviews, err := db.GetReviewsByPlatformID(ctx, platformID)
	require.NoError(t, err)
	require.Len(t, fetchedReviews, 2)

	byAuthor := make(map[string]domain.Review, len(fetchedReviews))
	for _, r := range fetchedReviews {
		byAuthor[r.Author] = r
	}
	require.Equal(t, "pc", byAuthor["IGN"].Platform)
	require.Equal(t, "", byAuthor["User1"].Platform)
}

func TestMigrate_RemovesMismatchedReviewPlatformRows(t *testing.T) {
	ctx := context.Background()
	dsn := t.TempDir() + "/migrate_reviews.db"

	// Первый запуск: создаем схему и данные
	db, err := storage.New(dsn)
	require.NoError(t, err)

	game := &domain.Game{
		Slug:  "mismatch-game",
		Title: "Mismatch Game",
		Platforms: []domain.GamePlatform{
			{Platform: "pc"},
			{Platform: "playstation-5"},
		},
	}
	require.NoError(t, db.UpsertGame(ctx, game))
	saved, err := db.GetGameBySlug(ctx, "mismatch-game")
	require.NoError(t, err)

	var pcID, ps5ID int64
	for _, p := range saved.Platforms {
		switch p.Platform {
		case "pc":
			pcID = p.ID
		case "playstation-5":
			ps5ID = p.ID
		}
	}
	require.NotZero(t, pcID)
	require.NotZero(t, ps5ID)
	require.NoError(t, db.Close())

	// Вставляем некорректные строки напрямую: отзыв PC под платформой PS5.
	// platform='' - легаси-строка, должна остаться.
	raw, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `
		INSERT INTO game_reviews (game_platform_id, review_type, author, score, text, content_hash, date_str, platform) VALUES
			(?, 'critic', 'WrongPlatform', NULL, 'PC review on PS5.', 'hash-wrong', '', 'pc'),
			(?, 'critic', 'LegacyCopy',   NULL, 'Legacy copy.',      'hash-legacy', '', ''),
			(?, 'critic', 'RightPlatform',NULL, 'PS5 review.',       'hash-right',  '', 'playstation-5');
	`, ps5ID, ps5ID, ps5ID)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	// Повторное открытие запускает миграцию
	db2, err := storage.New(dsn)
	require.NoError(t, err)
	defer db2.Close()

	revs, err := db2.GetReviewsByPlatformID(ctx, ps5ID)
	require.NoError(t, err)

	authors := make([]string, 0, len(revs))
	for _, r := range revs {
		authors = append(authors, r.Author)
	}
	require.ElementsMatch(t, []string{"LegacyCopy", "RightPlatform"}, authors)

	// Отзывы PC-платформы не задеты
	pcRevs, err := db2.GetReviewsByPlatformID(ctx, pcID)
	require.NoError(t, err)
	require.Empty(t, pcRevs)
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

func TestListGames_ReleaseDateSorting(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	// Добавляем игры с разными датами выпуска
	_ = db.UpsertGame(ctx, &domain.Game{
		ID: "g-old", Slug: "old-game", Title: "Old Game", ReleaseDate: "2020-01-15",
	})
	_ = db.UpsertGame(ctx, &domain.Game{
		ID: "g-new", Slug: "new-game", Title: "New Game", ReleaseDate: "2024-05-20",
	})
	_ = db.UpsertGame(ctx, &domain.Game{
		ID: "g-mid", Slug: "mid-game", Title: "Mid Game", ReleaseDate: "2022-11-10",
	})

	// Сортировка newest должна вернуть: New Game (2024), Mid Game (2022), Old Game (2020)
	games, err := db.ListGames(ctx, storage.ListFilter{Sort: "newest"})
	require.NoError(t, err)
	require.Len(t, games, 3)
	require.Equal(t, "New Game", games[0].Title)
	require.Equal(t, "2024-05-20", games[0].ReleaseDate)
	require.Equal(t, "Mid Game", games[1].Title)
	require.Equal(t, "2022-11-10", games[1].ReleaseDate)
	require.Equal(t, "Old Game", games[2].Title)
	require.Equal(t, "2020-01-15", games[2].ReleaseDate)
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

func TestCountGames_WithFilters(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	_ = db.UpsertGame(ctx, &domain.Game{
		ID: "g-1", Slug: "g-1", Title: "Elden Ring",
		Platforms: []domain.GamePlatform{{Platform: "pc"}, {Platform: "ps5"}},
	})
	_ = db.UpsertGame(ctx, &domain.Game{
		ID: "g-2", Slug: "g-2", Title: "Dark Souls 3",
		Platforms: []domain.GamePlatform{{Platform: "pc"}},
	})
	_ = db.UpsertGame(ctx, &domain.Game{
		ID: "g-3", Slug: "g-3", Title: "Bloodborne",
		Platforms: []domain.GamePlatform{{Platform: "ps4"}},
	})

	// Total count
	count, err := db.CountGames(ctx, storage.ListFilter{})
	require.NoError(t, err)
	require.Equal(t, 3, count)

	// Search count
	count, err = db.CountGames(ctx, storage.ListFilter{Search: "Souls"})
	require.NoError(t, err)
	require.Equal(t, 1, count)

	// Platform count
	count, err = db.CountGames(ctx, storage.ListFilter{Platform: "pc"})
	require.NoError(t, err)
	require.Equal(t, 2, count)

	// Search + Platform count
	count, err = db.CountGames(ctx, storage.ListFilter{Search: "Elden", Platform: "ps5"})
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestListGames_Pagination(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	for i := 1; i <= 5; i++ {
		_ = db.UpsertGame(ctx, &domain.Game{
			ID:    fmt.Sprintf("g-%d", i),
			Slug:  fmt.Sprintf("game-%d", i),
			Title: fmt.Sprintf("Game %d", i),
			ReleaseDate: fmt.Sprintf("2024-01-0%d", i),
		})
	}

	// Page 1 with limit 2
	p1, err := db.ListGames(ctx, storage.ListFilter{Limit: 2, Offset: 0, Sort: "newest"})
	require.NoError(t, err)
	require.Len(t, p1, 2)
	require.Equal(t, "Game 5", p1[0].Title)
	require.Equal(t, "Game 4", p1[1].Title)

	// Page 2 with limit 2
	p2, err := db.ListGames(ctx, storage.ListFilter{Limit: 2, Offset: 2, Sort: "newest"})
	require.NoError(t, err)
	require.Len(t, p2, 2)
	require.Equal(t, "Game 3", p2[0].Title)
	require.Equal(t, "Game 2", p2[1].Title)

	// Page 3 with limit 2
	p3, err := db.ListGames(ctx, storage.ListFilter{Limit: 2, Offset: 4, Sort: "newest"})
	require.NoError(t, err)
	require.Len(t, p3, 1)
	require.Equal(t, "Game 1", p3[0].Title)
}

