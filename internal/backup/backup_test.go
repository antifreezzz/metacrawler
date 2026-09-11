package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"metacrawler/internal/domain"
	"metacrawler/internal/storage"
)

func newSourceDB(t *testing.T, dir string) *storage.DB {
	t.Helper()
	db, err := storage.New(filepath.Join(dir, "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestBackupOnce_CreatesUsableSnapshot(t *testing.T) {
	dir := t.TempDir()
	db := newSourceDB(t, dir)
	ctx := context.Background()

	require.NoError(t, db.UpsertGame(ctx, &domain.Game{Slug: "snap-game", Title: "Snap"}))

	s := New(db, filepath.Join(dir, "backups"), time.Hour, 7)
	path, err := s.BackupOnce(ctx)
	require.NoError(t, err)
	require.FileExists(t, path)

	restored, err := storage.New(path)
	require.NoError(t, err)
	defer restored.Close()

	game, err := restored.GetGameBySlug(ctx, "snap-game")
	require.NoError(t, err)
	require.NotNil(t, game)
	require.Equal(t, "Snap", game.Title)
}

func TestPrune_KeepsOnlyNewest(t *testing.T) {
	dir := t.TempDir()
	db := newSourceDB(t, dir)
	backupDir := filepath.Join(dir, "backups")

	s := New(db, backupDir, time.Hour, 2)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := 0; i < 5; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		s.now = func() time.Time { return ts }
		_, err := s.BackupOnce(context.Background())
		require.NoError(t, err)
	}

	entries, err := os.ReadDir(backupDir)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.FileExists(t, filepath.Join(backupDir, "metacrawler-20260101-000300.db"))
	require.FileExists(t, filepath.Join(backupDir, "metacrawler-20260101-000400.db"))
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	db := newSourceDB(t, dir)
	backupDir := filepath.Join(dir, "backups")

	s := New(db, backupDir, 10*time.Millisecond, 3)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	require.Eventually(t, func() bool {
		entries, _ := os.ReadDir(backupDir)
		return len(entries) > 0
	}, time.Second, 10*time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Scheduler.Run did not stop after context cancellation")
	}
}
