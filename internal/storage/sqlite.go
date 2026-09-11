package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"metacrawler/internal/domain"
)

type DB struct {
	db *sql.DB
}

func New(dsn string) (*DB, error) {
	if dsn != ":memory:" && !strings.HasPrefix(dsn, "file::memory:") {
		dir := filepath.Dir(dsn)
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0700); err != nil {
				return nil, fmt.Errorf("create db directory %s: %w", dir, err)
			}
		}
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}

	// Ограничиваем пул 1 соединением для устранения конкуренции за писателя в SQLite
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// PRAGMA настройки: WAL режим, таймаут ожидания блокировки 5с, нормальная синхронизация
	if _, err := db.Exec(`
		PRAGMA busy_timeout = 5000;
		PRAGMA foreign_keys = ON;
		PRAGMA journal_mode = WAL;
		PRAGMA synchronous = NORMAL;
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply sqlite pragmas: %w", err)
	}

	storageDB := &DB{db: db}
	if err := storageDB.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate db: %w", err)
	}

	return storageDB, nil
}

func (d *DB) Close() error {
	return d.db.Close()
}

// Ping проверяет доступность БД (для readiness-проверки).
func (d *DB) Ping(ctx context.Context) error {
	return d.db.PingContext(ctx)
}

// migration - версионированная миграция схемы. Каждая применяется ровно один
// раз и фиксируется в таблице schema_migrations в той же транзакции.
type migration struct {
	version     int
	description string
	run         func(*sql.Tx) error
}

// migrations упорядочены по версии. Разрушительные чистки вынесены сюда, чтобы
// не выполняться при каждом старте приложения.
var migrations = []migration{
	{version: 1, description: "base schema", run: migrateBaseSchema},
	{version: 2, description: "clean inconsistent review platform rows", run: migrateReviewLineageCleanup},
	{version: 3, description: "clean fabricated summaries and stale youtube analyses", run: migrateFabricatedContentCleanup},
	{version: 4, description: "add youtube analysis status", run: migrateYouTubeAnalysisStatus},
	{version: 5, description: "drop fabricated all platform", run: migrateDropAllPlatform},
}

func (d *DB) migrate() error {
	if _, err := d.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		description TEXT NOT NULL DEFAULT '',
		applied_at DATETIME NOT NULL
	);`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	current, err := d.currentSchemaVersion()
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := d.applyMigration(m); err != nil {
			return fmt.Errorf("apply migration %d (%s): %w", m.version, m.description, err)
		}
	}
	return nil
}

// currentSchemaVersion возвращает максимальную применённую версию (0 для новой
// или легаси-БД без таблицы версий).
func (d *DB) currentSchemaVersion() (int, error) {
	var v sql.NullInt64
	if err := d.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

func (d *DB) applyMigration(m migration) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := m.run(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO schema_migrations (version, description, applied_at) VALUES (?, ?, ?)`,
		m.version, m.description, time.Now().UTC(),
	); err != nil {
		return err
	}
	return tx.Commit()
}

func columnExists(tx *sql.Tx, table, column string) (bool, error) {
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// ensureColumn добавляет колонку, если её ещё нет (для легаси-БД).
func ensureColumn(tx *sql.Tx, table, column, alter string) error {
	exists, err := columnExists(tx, table, column)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = tx.Exec(alter)
	return err
}

func migrateBaseSchema(tx *sql.Tx) error {
	ddl := `
	CREATE TABLE IF NOT EXISTS games (
		id TEXT PRIMARY KEY,
		slug TEXT UNIQUE NOT NULL,
		title TEXT NOT NULL,
		cover_url TEXT NOT NULL DEFAULT '',
		developer TEXT NOT NULL DEFAULT '',
		description TEXT NOT NULL DEFAULT '',
		description_ru TEXT NOT NULL DEFAULT '',
		video_url TEXT NOT NULL DEFAULT '',
		release_date TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS game_platforms (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		game_id TEXT NOT NULL REFERENCES games(id) ON DELETE CASCADE,
		platform TEXT NOT NULL,
		metascore INTEGER,
		userscore REAL,
		platform_url TEXT NOT NULL DEFAULT '',
		updated_at DATETIME NOT NULL,
		UNIQUE(game_id, platform)
	);

	CREATE TABLE IF NOT EXISTS game_reviews (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		game_platform_id INTEGER NOT NULL REFERENCES game_platforms(id) ON DELETE CASCADE,
		review_type TEXT NOT NULL,
		author TEXT NOT NULL,
		score REAL,
		text TEXT NOT NULL,
		content_hash TEXT NOT NULL,
		date_str TEXT NOT NULL DEFAULT '',
		platform TEXT NOT NULL DEFAULT '',
		UNIQUE(game_platform_id, content_hash)
	);

	CREATE TABLE IF NOT EXISTS platform_summaries (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		game_platform_id INTEGER NOT NULL REFERENCES game_platforms(id) ON DELETE CASCADE,
		critic_pros TEXT NOT NULL DEFAULT '',
		critic_cons TEXT NOT NULL DEFAULT '',
		user_pros TEXT NOT NULL DEFAULT '',
		user_cons TEXT NOT NULL DEFAULT '',
		updated_at DATETIME NOT NULL,
		UNIQUE(game_platform_id)
	);

	CREATE TABLE IF NOT EXISTS crawl_history (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		slug TEXT NOT NULL,
		date_str TEXT NOT NULL,
		crawled_at DATETIME NOT NULL,
		UNIQUE(slug, date_str)
	);

	CREATE TABLE IF NOT EXISTS crawler_state (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS game_embeddings (
		game_id TEXT PRIMARY KEY REFERENCES games(id) ON DELETE CASCADE,
		vector BLOB NOT NULL,
		dimensions INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS youtube_analyses (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		game_id TEXT NOT NULL REFERENCES games(id) ON DELETE CASCADE,
		video_id TEXT NOT NULL,
		video_title TEXT NOT NULL,
		video_url TEXT NOT NULL,
		channel_name TEXT NOT NULL DEFAULT '',
		view_count INTEGER NOT NULL DEFAULT 0,
		summary TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'analyzed',
		created_at DATETIME NOT NULL,
		UNIQUE(game_id)
	);

	CREATE TABLE IF NOT EXISTS score_history (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		game_platform_id INTEGER NOT NULL REFERENCES game_platforms(id) ON DELETE CASCADE,
		metascore INTEGER,
		userscore REAL,
		recorded_at DATETIME NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_games_slug ON games(slug);
	CREATE INDEX IF NOT EXISTS idx_crawl_history_lookup ON crawl_history(slug, date_str);
	CREATE INDEX IF NOT EXISTS idx_game_platforms_game_id ON game_platforms(game_id);
	CREATE INDEX IF NOT EXISTS idx_score_history_platform ON score_history(game_platform_id, recorded_at);
	`
	if _, err := tx.Exec(ddl); err != nil {
		return err
	}

	if err := ensureColumn(tx, "games", "release_date", `ALTER TABLE games ADD COLUMN release_date TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(tx, "game_reviews", "platform", `ALTER TABLE game_reviews ADD COLUMN platform TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(tx, "games", "description_ru", `ALTER TABLE games ADD COLUMN description_ru TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}

	return nil
}

func migrateReviewLineageCleanup(tx *sql.Tx) error {

	// Чистка некорректно привязанных отзывов: строка с известной платформой,
	// лежащая под другой платформой игры (артефакт копирования отзывов во все платформы).
	if _, err := tx.Exec(`
		DELETE FROM game_reviews
		WHERE platform != ''
		  AND platform != (SELECT gp.platform FROM game_platforms gp WHERE gp.id = game_reviews.game_platform_id);
	`); err != nil {
		return err
	}

	// Чистка легаси-отзывов старого парсера: в текст склеивались дата, оценка и автор
	// ("Jan 5, 2024100 Parse\"Superb\""), а одна строка копировалась во все платформы.
	// Такие строки не перезаписываются пересбором (другой content_hash), поэтому удаляются:
	// ближайший цикл сбора вернет те же отзывы в чистом виде.
	if _, err := tx.Exec(`
		DELETE FROM game_reviews
		WHERE text GLOB '[A-Z][a-z][a-z] [0-9], [0-9][0-9][0-9][0-9][0-9]*'
		   OR text GLOB '[A-Z][a-z][a-z] [0-9][0-9], [0-9][0-9][0-9][0-9][0-9]*'
		   OR text GLOB '[A-Z][a-z][a-z] [0-9], [0-9][0-9][0-9][0-9][A-Za-z]*'
		   OR text GLOB '[A-Z][a-z][a-z] [0-9][0-9], [0-9][0-9][0-9][0-9][A-Za-z]*';
	`); err != nil {
		return err
	}
	// Схлопывание остаточных копий одного отзыва на нескольких платформах:
	// в новой схеме отзыв живет только в своей платформе.
	if _, err := tx.Exec(`
		DELETE FROM game_reviews
		WHERE content_hash IN (
			SELECT content_hash FROM game_reviews
			GROUP BY content_hash HAVING COUNT(DISTINCT game_platform_id) > 1
		) AND id NOT IN (
			SELECT MIN(id) FROM game_reviews
			GROUP BY content_hash
		);
	`); err != nil {
		return err
	}

	return nil
}

func migrateFabricatedContentCleanup(tx *sql.Tx) error {
	// Чистка выдуманных резюме из старого фоллбэка (когда LLM была недоступна,
	// генерировался шаблонный текст, нарушающий принцип честности данных).
	if _, err := tx.Exec(`
		DELETE FROM platform_summaries
		WHERE critic_pros LIKE '%высокое качество графики и проработку игрового мира%'
		   OR critic_cons LIKE '%отдельные огрехи оптимизации и сложность освоения%'
		   OR user_pros LIKE '%Игрокам нравится атмосфера, динамика и увлекательный сюжет%'
		   OR user_cons LIKE '%жалуются на баланс и технические шероховатости%';
	`); err != nil {
		return err
	}

	// Очистка ошибочно прикрепленных нерелевантных видео и шаблонных заглушек из прошлых запусков
	if _, err := tx.Exec(`
		DELETE FROM youtube_analyses
		WHERE summary LIKE '%исследует ключевые механики, боевую систему%'
		   OR game_id IN (
			SELECT g.id FROM games g
			WHERE (g.slug = 'ant-simulator-stock-market-game' AND LOWER(video_title) LIKE '%pocket ants%')
			   OR (g.slug = 'escape-from-company' AND LOWER(video_title) LIKE '%star wars%')
		);
	`); err != nil {
		return err
	}

	return nil
}

func migrateYouTubeAnalysisStatus(tx *sql.Tx) error {
	// Легаси-строки считаются полноценным анализом; новые могут быть no_transcript.
	return ensureColumn(tx, "youtube_analyses", "status", `ALTER TABLE youtube_analyses ADD COLUMN status TEXT NOT NULL DEFAULT 'analyzed'`)
}

func migrateDropAllPlatform(tx *sql.Tx) error {
	// "all" была искусственной платформой, привязка к ней не несет данных;
	// связанные отзывы/резюме/история удаляются каскадом.
	if _, err := tx.Exec(`DELETE FROM game_platforms WHERE platform = 'all'`); err != nil {
		return err
	}
	return nil
}

func (d *DB) UpsertGame(ctx context.Context, game *domain.Game) error {
	now := time.Now().UTC()
	if game.ID == "" {
		game.ID = uuid.NewString()
	}
	if game.CreatedAt.IsZero() {
		game.CreatedAt = now
	}
	game.UpdatedAt = now

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Определяем, нужно ли инвалидировать кэш перевода описания.
	// Сравниваем описание по нормализованному виду, чтобы косметические различия
	// от скрейпера (пробелы, переносы) не сбрасывали перевод каждый цикл.
	var existingDesc, existingRU string
	scanErr := tx.QueryRowContext(ctx, `SELECT description, description_ru FROM games WHERE slug = ?`, game.Slug).
		Scan(&existingDesc, &existingRU)
	if scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
		return fmt.Errorf("read existing game description: %w", scanErr)
	}
	newDescriptionRU := existingRU
	if game.Description != "" && normalizeSpaces(game.Description) != normalizeSpaces(existingDesc) {
		newDescriptionRU = ""
	}
	game.DescriptionRU = newDescriptionRU

	// 1. Upsert game
	queryGame := `
	INSERT INTO games (id, slug, title, cover_url, developer, description, description_ru, video_url, release_date, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(slug) DO UPDATE SET
		title = excluded.title,
		cover_url = CASE WHEN excluded.cover_url != '' THEN excluded.cover_url ELSE games.cover_url END,
		developer = CASE WHEN excluded.developer != '' THEN excluded.developer ELSE games.developer END,
		description = CASE WHEN excluded.description != '' THEN excluded.description ELSE games.description END,
		description_ru = excluded.description_ru,
		video_url = CASE WHEN excluded.video_url != '' THEN excluded.video_url ELSE games.video_url END,
		release_date = CASE WHEN excluded.release_date != '' THEN excluded.release_date ELSE games.release_date END,
		updated_at = excluded.updated_at
	RETURNING id;
	`
	err = tx.QueryRowContext(ctx, queryGame,
		game.ID, game.Slug, game.Title, game.CoverURL, game.Developer, game.Description, game.DescriptionRU, game.VideoURL, game.ReleaseDate, game.CreatedAt, game.UpdatedAt,
	).Scan(&game.ID)
	if err != nil {
		return fmt.Errorf("upsert game row: %w", err)
	}

	// 2. Upsert platforms
	for i := range game.Platforms {
		p := &game.Platforms[i]
		p.GameID = game.ID
		p.UpdatedAt = now

		queryPlatform := `
		INSERT INTO game_platforms (game_id, platform, metascore, userscore, platform_url, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(game_id, platform) DO UPDATE SET
			metascore = excluded.metascore,
			userscore = excluded.userscore,
			platform_url = CASE WHEN excluded.platform_url != '' THEN excluded.platform_url ELSE game_platforms.platform_url END,
			updated_at = excluded.updated_at
		RETURNING id;
		`
		err = tx.QueryRowContext(ctx, queryPlatform,
			p.GameID, p.Platform, p.Metascore, p.Userscore, p.PlatformURL, p.UpdatedAt,
		).Scan(&p.ID)
		if err != nil {
			return fmt.Errorf("upsert platform row (%s): %w", p.Platform, err)
		}
	}

	return tx.Commit()
}

// normalizeSpaces схлопывает последовательности пробельных символов в один пробел
// и обрезает края. Нужен для устойчивого сравнения описаний из разных скрейпов.
func normalizeSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// SaveGameTranslation сохраняет русский перевод описания игры.
func (d *DB) SaveGameTranslation(ctx context.Context, gameID, descriptionRU string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE games SET description_ru = ? WHERE id = ?`,
		strings.TrimSpace(descriptionRU), gameID)
	if err != nil {
		return fmt.Errorf("save game translation: %w", err)
	}
	return nil
}

func (d *DB) GetGameBySlug(ctx context.Context, slug string) (*domain.Game, error) {
	query := `
	SELECT id, slug, title, cover_url, developer, description, description_ru, video_url, release_date, created_at, updated_at
	FROM games WHERE slug = ?;
	`
	var game domain.Game
	err := d.db.QueryRowContext(ctx, query, slug).Scan(
		&game.ID, &game.Slug, &game.Title, &game.CoverURL, &game.Developer, &game.Description, &game.DescriptionRU, &game.VideoURL, &game.ReleaseDate, &game.CreatedAt, &game.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	platforms, err := d.getPlatformsForGame(ctx, game.ID)
	if err != nil {
		return nil, err
	}
	game.Platforms = platforms

	return &game, nil
}

func (d *DB) GetGameByID(ctx context.Context, id string) (*domain.Game, error) {
	query := `
	SELECT id, slug, title, cover_url, developer, description, description_ru, video_url, release_date, created_at, updated_at
	FROM games WHERE id = ?;
	`
	var game domain.Game
	err := d.db.QueryRowContext(ctx, query, id).Scan(
		&game.ID, &game.Slug, &game.Title, &game.CoverURL, &game.Developer, &game.Description, &game.DescriptionRU, &game.VideoURL, &game.ReleaseDate, &game.CreatedAt, &game.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	platforms, err := d.getPlatformsForGame(ctx, game.ID)
	if err != nil {
		return nil, err
	}
	game.Platforms = platforms

	return &game, nil
}

type ListFilter struct {
	Search   string
	Platform string
	Sort     string // metascore_desc, userscore_desc, newest, title_asc
	Limit    int
	Offset   int
}

func (d *DB) CountGames(ctx context.Context, filter ListFilter) (int, error) {
	whereClauses := []string{"1=1"}
	var args []interface{}

	if strings.TrimSpace(filter.Search) != "" {
		whereClauses = append(whereClauses, "LOWER(g.title) LIKE ?")
		args = append(args, "%"+strings.ToLower(strings.TrimSpace(filter.Search))+"%")
	}

	if strings.TrimSpace(filter.Platform) != "" {
		whereClauses = append(whereClauses, "EXISTS (SELECT 1 FROM game_platforms gp WHERE gp.game_id = g.id AND LOWER(gp.platform) = ?)")
		args = append(args, strings.ToLower(strings.TrimSpace(filter.Platform)))
	}

	query := fmt.Sprintf(`
		SELECT COUNT(*)
		FROM games g
		WHERE %s;
	`, strings.Join(whereClauses, " AND "))

	var count int
	if err := d.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("count games query: %w", err)
	}
	return count, nil
}

func (d *DB) ListGames(ctx context.Context, filter ListFilter) ([]domain.Game, error) {
	if filter.Limit <= 0 {
		filter.Limit = 100
	}

	whereClauses := []string{"1=1"}
	var args []interface{}

	if strings.TrimSpace(filter.Search) != "" {
		whereClauses = append(whereClauses, "LOWER(g.title) LIKE ?")
		args = append(args, "%"+strings.ToLower(strings.TrimSpace(filter.Search))+"%")
	}

	if strings.TrimSpace(filter.Platform) != "" {
		whereClauses = append(whereClauses, "EXISTS (SELECT 1 FROM game_platforms gp WHERE gp.game_id = g.id AND LOWER(gp.platform) = ?)")
		args = append(args, strings.ToLower(strings.TrimSpace(filter.Platform)))
	}

	orderClause := "CASE WHEN g.release_date != '' THEN g.release_date ELSE '1970-01-01' END DESC, g.created_at DESC"
	switch filter.Sort {
	case "metascore_desc":
		orderClause = "(SELECT MAX(gp.metascore) FROM game_platforms gp WHERE gp.game_id = g.id) DESC NULLS LAST"
	case "userscore_desc":
		orderClause = "(SELECT MAX(gp.userscore) FROM game_platforms gp WHERE gp.game_id = g.id) DESC NULLS LAST"
	case "title_asc":
		orderClause = "g.title COLLATE NOCASE ASC"
	case "newest":
		orderClause = "CASE WHEN g.release_date != '' THEN g.release_date ELSE '1970-01-01' END DESC, g.created_at DESC"
	}

	query := fmt.Sprintf(`
		SELECT g.id, g.slug, g.title, g.cover_url, g.developer, g.description, g.description_ru, g.video_url, g.release_date, g.created_at, g.updated_at
		FROM games g
		WHERE %s
		ORDER BY %s
		LIMIT ? OFFSET ?;
	`, strings.Join(whereClauses, " AND "), orderClause)

	args = append(args, filter.Limit, filter.Offset)

	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list games query: %w", err)
	}
	defer rows.Close()

	var games []domain.Game
	for rows.Next() {
		var g domain.Game
		if err := rows.Scan(&g.ID, &g.Slug, &g.Title, &g.CoverURL, &g.Developer, &g.Description, &g.DescriptionRU, &g.VideoURL, &g.ReleaseDate, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, err
		}
		games = append(games, g)
	}

	// Подтягиваем платформы для каждой игры
	for i := range games {
		platforms, err := d.getPlatformsForGame(ctx, games[i].ID)
		if err == nil {
			games[i].Platforms = platforms
		}
	}

	return games, rows.Err()
}

func (d *DB) GetDistinctPlatforms(ctx context.Context) ([]string, error) {
	query := `SELECT DISTINCT platform FROM game_platforms WHERE platform != '' ORDER BY platform ASC;`
	rows, err := d.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	return list, rows.Err()
}

func (d *DB) getPlatformsForGame(ctx context.Context, gameID string) ([]domain.GamePlatform, error) {
	query := `
	SELECT id, game_id, platform, metascore, userscore, platform_url, updated_at
	FROM game_platforms WHERE game_id = ?
	ORDER BY platform ASC;
	`
	rows, err := d.db.QueryContext(ctx, query, gameID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []domain.GamePlatform
	for rows.Next() {
		var p domain.GamePlatform
		if err := rows.Scan(&p.ID, &p.GameID, &p.Platform, &p.Metascore, &p.Userscore, &p.PlatformURL, &p.UpdatedAt); err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	return list, rows.Err()
}

func (d *DB) SaveReviews(ctx context.Context, reviews []domain.Review) (int, error) {
	if len(reviews) == 0 {
		return 0, nil
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
	INSERT OR IGNORE INTO game_reviews (game_platform_id, review_type, author, score, text, content_hash, date_str, platform)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?);
	`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	savedCount := 0
	for _, r := range reviews {
		if r.ContentHash == "" {
			r.ContentHash = domain.ComputeContentHash(r.ReviewType, r.Author, r.Text)
		}
		res, err := stmt.ExecContext(ctx, r.GamePlatformID, string(r.ReviewType), r.Author, r.Score, r.Text, r.ContentHash, r.DateStr, r.Platform)
		if err != nil {
			return 0, err
		}
		affected, _ := res.RowsAffected()
		if affected > 0 {
			savedCount++
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return savedCount, nil
}

func (d *DB) GetReviewsByPlatformID(ctx context.Context, platformID int64) ([]domain.Review, error) {
	query := `
	SELECT id, game_platform_id, review_type, author, score, text, content_hash, date_str, platform
	FROM game_reviews WHERE game_platform_id = ?
	ORDER BY id ASC;
	`
	rows, err := d.db.QueryContext(ctx, query, platformID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []domain.Review
	for rows.Next() {
		var r domain.Review
		var rType string
		if err := rows.Scan(&r.ID, &r.GamePlatformID, &rType, &r.Author, &r.Score, &r.Text, &r.ContentHash, &r.DateStr, &r.Platform); err != nil {
			return nil, err
		}
		r.ReviewType = domain.ReviewType(rType)
		list = append(list, r)
	}
	return list, rows.Err()
}

func (d *DB) UpsertPlatformSummary(ctx context.Context, summary *domain.PlatformSummary) error {
	summary.UpdatedAt = time.Now().UTC()
	query := `
	INSERT INTO platform_summaries (game_platform_id, critic_pros, critic_cons, user_pros, user_cons, updated_at)
	VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(game_platform_id) DO UPDATE SET
		critic_pros = excluded.critic_pros,
		critic_cons = excluded.critic_cons,
		user_pros = excluded.user_pros,
		user_cons = excluded.user_cons,
		updated_at = excluded.updated_at
	RETURNING id;
	`
	return d.db.QueryRowContext(ctx, query,
		summary.GamePlatformID, summary.CriticPros, summary.CriticCons, summary.UserPros, summary.UserCons, summary.UpdatedAt,
	).Scan(&summary.ID)
}

func (d *DB) GetPlatformSummary(ctx context.Context, platformID int64) (*domain.PlatformSummary, error) {
	query := `
	SELECT id, game_platform_id, critic_pros, critic_cons, user_pros, user_cons, updated_at
	FROM platform_summaries WHERE game_platform_id = ?;
	`
	var s domain.PlatformSummary
	err := d.db.QueryRowContext(ctx, query, platformID).Scan(
		&s.ID, &s.GamePlatformID, &s.CriticPros, &s.CriticCons, &s.UserPros, &s.UserCons, &s.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// RecordScorePoint фиксирует значения оценок платформы, если они отличаются от последней записи.
func (d *DB) RecordScorePoint(ctx context.Context, platformID int64, metascore *int, userscore *float64) error {
	latest, err := d.latestScorePoint(ctx, platformID)
	if err != nil {
		return err
	}
	if latest != nil &&
		scorePtrEqual(latest.Metascore, metascore) &&
		scorePtrEqual(latest.Userscore, userscore) {
		return nil
	}

	query := `
	INSERT INTO score_history (game_platform_id, metascore, userscore, recorded_at)
	VALUES (?, ?, ?, ?);
	`
	_, err = d.db.ExecContext(ctx, query, platformID, metascore, userscore, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("record score point: %w", err)
	}
	return nil
}

func (d *DB) latestScorePoint(ctx context.Context, platformID int64) (*domain.ScorePoint, error) {
	var p domain.ScorePoint
	var metascore sql.NullInt64
	var userscore sql.NullFloat64
	var recordedAt time.Time
	err := d.db.QueryRowContext(ctx, `
		SELECT metascore, userscore, recorded_at FROM score_history
		WHERE game_platform_id = ?
		ORDER BY recorded_at DESC, id DESC LIMIT 1;
	`, platformID).Scan(&metascore, &userscore, &recordedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if metascore.Valid {
		v := int(metascore.Int64)
		p.Metascore = &v
	}
	if userscore.Valid {
		v := userscore.Float64
		p.Userscore = &v
	}
	p.RecordedAt = recordedAt
	return &p, nil
}

func scorePtrEqual[T comparable](a, b *T) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func (d *DB) GetScoreHistory(ctx context.Context, platformID int64) ([]domain.ScorePoint, error) {
	query := `
	SELECT metascore, userscore, recorded_at
	FROM score_history WHERE game_platform_id = ?
	ORDER BY recorded_at ASC, id ASC;
	`
	rows, err := d.db.QueryContext(ctx, query, platformID)
	if err != nil {
		return nil, fmt.Errorf("get score history: %w", err)
	}
	defer rows.Close()

	var list []domain.ScorePoint
	for rows.Next() {
		var p domain.ScorePoint
		var metascore sql.NullInt64
		var userscore sql.NullFloat64
		if err := rows.Scan(&metascore, &userscore, &p.RecordedAt); err != nil {
			return nil, err
		}
		if metascore.Valid {
			v := int(metascore.Int64)
			p.Metascore = &v
		}
		if userscore.Valid {
			v := userscore.Float64
			p.Userscore = &v
		}
		p.GamePlatformID = platformID
		list = append(list, p)
	}
	return list, rows.Err()
}

func (d *DB) IsProcessedOnDate(ctx context.Context, slug, dateStr string) (bool, error) {
	query := `SELECT 1 FROM crawl_history WHERE slug = ? AND date_str = ? LIMIT 1;`
	var dummy int
	err := d.db.QueryRowContext(ctx, query, slug, dateStr).Scan(&dummy)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (d *DB) MarkProcessed(ctx context.Context, slug, dateStr string) error {
	query := `
	INSERT OR IGNORE INTO crawl_history (slug, date_str, crawled_at)
	VALUES (?, ?, ?);
	`
	_, err := d.db.ExecContext(ctx, query, slug, dateStr, time.Now().UTC())
	return err
}

func (d *DB) GetState(ctx context.Context, key string) (string, error) {
	query := `SELECT value FROM crawler_state WHERE key = ?;`
	var val string
	err := d.db.QueryRowContext(ctx, query, key).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return val, nil
}

func (d *DB) SetState(ctx context.Context, key, value string) error {
	query := `
	INSERT INTO crawler_state (key, value, updated_at)
	VALUES (?, ?, ?)
	ON CONFLICT(key) DO UPDATE SET
		value = excluded.value,
		updated_at = excluded.updated_at;
	`
	_, err := d.db.ExecContext(ctx, query, key, value, time.Now().UTC())
	return err
}

func (d *DB) SaveEmbedding(ctx context.Context, emb *domain.GameEmbedding) error {
	blob := encodeVector(emb.Vector)
	query := `
	INSERT INTO game_embeddings (game_id, vector, dimensions)
	VALUES (?, ?, ?)
	ON CONFLICT(game_id) DO UPDATE SET
		vector = excluded.vector,
		dimensions = excluded.dimensions;
	`
	_, err := d.db.ExecContext(ctx, query, emb.GameID, blob, emb.Dimensions)
	return err
}

func (d *DB) GetAllEmbeddings(ctx context.Context) ([]domain.GameEmbedding, error) {
	query := `SELECT game_id, vector, dimensions FROM game_embeddings;`
	rows, err := d.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []domain.GameEmbedding
	for rows.Next() {
		var emb domain.GameEmbedding
		var blob []byte
		if err := rows.Scan(&emb.GameID, &blob, &emb.Dimensions); err != nil {
			return nil, err
		}
		emb.Vector = decodeVector(blob)
		list = append(list, emb)
	}
	return list, rows.Err()
}

func (d *DB) UpsertYouTubeAnalysis(ctx context.Context, y *domain.YouTubeAnalysis) error {
	y.CreatedAt = time.Now().UTC()
	if y.Status == "" {
		y.Status = domain.YouTubeStatusAnalyzed
	}
	query := `
	INSERT INTO youtube_analyses (game_id, video_id, video_title, video_url, channel_name, view_count, summary, status, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(game_id) DO UPDATE SET
		video_id = excluded.video_id,
		video_title = excluded.video_title,
		video_url = excluded.video_url,
		channel_name = excluded.channel_name,
		view_count = excluded.view_count,
		summary = excluded.summary,
		status = excluded.status,
		created_at = excluded.created_at
	RETURNING id;
	`
	return d.db.QueryRowContext(ctx, query,
		y.GameID, y.VideoID, y.VideoTitle, y.VideoURL, y.ChannelName, y.ViewCount, y.Summary, y.Status, y.CreatedAt,
	).Scan(&y.ID)
}

func (d *DB) GetYouTubeAnalysis(ctx context.Context, gameID string) (*domain.YouTubeAnalysis, error) {
	query := `
	SELECT id, game_id, video_id, video_title, video_url, channel_name, view_count, summary, status, created_at
	FROM youtube_analyses WHERE game_id = ?;
	`
	var y domain.YouTubeAnalysis
	err := d.db.QueryRowContext(ctx, query, gameID).Scan(
		&y.ID, &y.GameID, &y.VideoID, &y.VideoTitle, &y.VideoURL, &y.ChannelName, &y.ViewCount, &y.Summary, &y.Status, &y.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &y, nil
}

func (d *DB) DeleteYouTubeAnalysis(ctx context.Context, gameID string) error {
	query := `DELETE FROM youtube_analyses WHERE game_id = ?;`
	_, err := d.db.ExecContext(ctx, query, gameID)
	return err
}

func encodeVector(vec []float32) []byte {
	buf := new(bytes.Buffer)
	for _, f := range vec {
		bits := math.Float32bits(f)
		_ = binary.Write(buf, binary.LittleEndian, bits)
	}
	return buf.Bytes()
}

func decodeVector(b []byte) []float32 {
	count := len(b) / 4
	vec := make([]float32, count)
	buf := bytes.NewReader(b)
	for i := 0; i < count; i++ {
		var bits uint32
		_ = binary.Read(buf, binary.LittleEndian, &bits)
		vec[i] = math.Float32frombits(bits)
	}
	return vec
}
