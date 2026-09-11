package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"metacrawler/internal/domain"
	"metacrawler/internal/storage"
)

func TestCachedEmbeddings_ReusesWithinTTL(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(":memory:")
	require.NoError(t, err)
	defer db.Close()

	require.NoError(t, db.UpsertGame(ctx, &domain.Game{ID: "g1", Slug: "g1", Title: "G1"}))
	require.NoError(t, db.SaveEmbedding(ctx, &domain.GameEmbedding{GameID: "g1", Vector: []float32{1, 0}, Dimensions: 2}))

	s := &Server{db: db}

	first := s.cachedEmbeddings(ctx)
	require.Len(t, first, 1)

	// Новый вектор в пределах TTL не подхватывается: кэш не перечитывает таблицу.
	require.NoError(t, db.UpsertGame(ctx, &domain.Game{ID: "g2", Slug: "g2", Title: "G2"}))
	require.NoError(t, db.SaveEmbedding(ctx, &domain.GameEmbedding{GameID: "g2", Vector: []float32{0, 1}, Dimensions: 2}))

	second := s.cachedEmbeddings(ctx)
	require.Len(t, second, 1, "в пределах TTL кэш не перечитывается")

	// После истечения TTL кэш обновляется.
	s.embCache.expires = time.Now().Add(-time.Second)
	third := s.cachedEmbeddings(ctx)
	require.Len(t, third, 2)
}
