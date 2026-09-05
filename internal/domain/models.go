package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

type ReviewType string

const (
	ReviewTypeCritic ReviewType = "critic"
	ReviewTypeUser   ReviewType = "user"
)

// Game представляет базовую сущность игры.
type Game struct {
	ID          string         `json:"id"`
	Slug        string         `json:"slug"`
	Title       string         `json:"title"`
	CoverURL    string         `json:"cover_url"`
	Developer   string         `json:"developer"`
	Description string         `json:"description"`
	VideoURL    string         `json:"video_url"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	Platforms   []GamePlatform `json:"platforms,omitempty"`
}

// GamePlatform хранит оценки и данные игры для конкретной платформы (PC, PS5, Xbox Series X и т.д.).
type GamePlatform struct {
	ID          int64            `json:"id"`
	GameID      string           `json:"game_id"`
	Platform    string           `json:"platform"`
	Metascore   *int             `json:"metascore,omitempty"` // nil если TBD
	Userscore   *float64         `json:"userscore,omitempty"` // nil если TBD
	PlatformURL string           `json:"platform_url"`
	UpdatedAt   time.Time        `json:"updated_at"`
	Reviews     []Review         `json:"reviews,omitempty"`
	Summary     *PlatformSummary `json:"summary,omitempty"`
}

// Review представляет отдельный отзыв критика или пользователя.
type Review struct {
	ID             int64      `json:"id"`
	GamePlatformID int64      `json:"game_platform_id"`
	ReviewType     ReviewType `json:"review_type"`
	Author         string     `json:"author"`
	Score          *float64   `json:"score,omitempty"`
	Text           string     `json:"text"`
	ContentHash    string     `json:"content_hash"`
	DateStr        string     `json:"date_str"`
}

// ComputeContentHash вычисляет SHA-256 хеш от автора и нормализованного текста для дедубликации отзывов.
func ComputeContentHash(reviewType ReviewType, author, text string) string {
	raw := fmt.Sprintf("%s:%s:%s", reviewType, strings.TrimSpace(author), strings.TrimSpace(text))
	hash := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(hash[:])
}

// PlatformSummary содержит сгенерированное LLM раздельное резюме отзывов для конкретной платформы.
type PlatformSummary struct {
	ID             int64     `json:"id"`
	GamePlatformID int64     `json:"game_platform_id"`
	CriticPros     string    `json:"critic_pros"`
	CriticCons     string    `json:"critic_cons"`
	UserPros       string    `json:"user_pros"`
	UserCons       string    `json:"user_cons"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// GameEmbedding хранит векторное представление игры для поиска похожих.
type GameEmbedding struct {
	GameID     string    `json:"game_id"`
	Vector     []float32 `json:"vector"`
	Dimensions int       `json:"dimensions"`
}
