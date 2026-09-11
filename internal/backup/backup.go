// Package backup periodically snapshots the SQLite database and enforces a
// retention policy. Snapshots use VACUUM INTO, so they are consistent even
// while the service is running.
package backup

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const filePrefix = "metacrawler-"

// DB - минимальный интерфейс хранилища, нужный для бэкапа.
type DB interface {
	BackupTo(ctx context.Context, path string) error
}

type Scheduler struct {
	db        DB
	dir       string
	interval  time.Duration
	retention int
	now       func() time.Time
}

func New(db DB, dir string, interval time.Duration, retention int) *Scheduler {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	if retention <= 0 {
		retention = 7
	}
	return &Scheduler{
		db:        db,
		dir:       dir,
		interval:  interval,
		retention: retention,
		now:       time.Now,
	}
}

// Run делает первый бэкап сразу, затем повторяет по интервалу до отмены ctx.
func (s *Scheduler) Run(ctx context.Context) {
	if path, err := s.BackupOnce(ctx); err != nil {
		slog.Error("backup failed", "error", err)
	} else {
		slog.Info("backup created", "path", path)
	}

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if path, err := s.BackupOnce(ctx); err != nil {
				slog.Error("backup failed", "error", err)
			} else {
				slog.Info("backup created", "path", path)
			}
		}
	}
}

// BackupOnce создает один снапшот и применяет retention.
func (s *Scheduler) BackupOnce(ctx context.Context) (string, error) {
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return "", fmt.Errorf("create backup dir: %w", err)
	}

	name := filePrefix + s.now().UTC().Format("20060102-150405") + ".db"
	path := filepath.Join(s.dir, name)

	// VACUUM INTO не перезаписывает существующий файл.
	if _, err := os.Stat(path); err == nil {
		if err := os.Remove(path); err != nil {
			return "", fmt.Errorf("remove existing backup: %w", err)
		}
	}

	if err := s.db.BackupTo(ctx, path); err != nil {
		return "", err
	}
	if err := s.prune(); err != nil {
		slog.Warn("backup retention failed", "error", err)
	}
	return path, nil
}

// prune оставляет только retention самых свежих снапшотов.
func (s *Scheduler) prune() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), filePrefix) && strings.HasSuffix(e.Name(), ".db") {
			names = append(names, e.Name())
		}
	}
	if len(names) <= s.retention {
		return nil
	}

	sort.Strings(names)
	for _, name := range names[:len(names)-s.retention] {
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil {
			return err
		}
	}
	return nil
}
