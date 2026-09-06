package scraper

import (
	"bytes"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/tidwall/gjson"

	"metacrawler/internal/domain"
)

// ExtractSlugFromURL извлекает слаг игры из ссылки вида /game/elden-ring/ или https://www.metacritic.com/game/elden-ring
func ExtractSlugFromURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	u, err := url.Parse(rawURL)
	if err == nil && u.Path != "" {
		rawURL = u.Path
	}
	parts := strings.Split(strings.Trim(rawURL, "/"), "/")
	for i, p := range parts {
		if p == "game" && i+1 < len(parts) {
			candidate := parts[i+1]
			if candidate != "" && candidate != "browse" {
				return candidate
			}
		}
	}
	return ""
}

func ParseNewReleases(data []byte) ([]string, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	var slugs []string

	// 1. Приоритетный селектор для карусели New Releases
	doc.Find("[data-testid='new-game-release-carousel'] a[href*='/game/']").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists || strings.Contains(href, "/browse/") {
			return
		}
		slug := ExtractSlugFromURL(href)
		if slug != "" && !seen[slug] {
			seen[slug] = true
			slugs = append(slugs, slug)
		}
	})

	// 2. Если в карусели меньше 20, fallback по другим карточкам на странице
	if len(slugs) < 20 {
		doc.Find("a[href*='/game/']").Each(func(i int, s *goquery.Selection) {
			if len(slugs) >= 20 {
				return
			}
			href, exists := s.Attr("href")
			if !exists || strings.Contains(href, "/browse/") {
				return
			}
			slug := ExtractSlugFromURL(href)
			if slug != "" && !seen[slug] {
				seen[slug] = true
				slugs = append(slugs, slug)
			}
		})
	}

	if len(slugs) > 20 {
		slugs = slugs[:20]
	}
	return slugs, nil
}

func ParseBrowseCatalog(data []byte) ([]string, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	var slugs []string

	// 1. Карточки результатов фильтрации каталога
	doc.Find("[data-testid='filter-results'] a[href*='/game/']").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists || strings.Contains(href, "/browse/") {
			return
		}
		slug := ExtractSlugFromURL(href)
		if slug != "" && !seen[slug] {
			seen[slug] = true
			slugs = append(slugs, slug)
		}
	})

	// 2. Fallback если селектор не найден
	if len(slugs) == 0 {
		doc.Find("a[href*='/game/']").Each(func(i int, s *goquery.Selection) {
			href, exists := s.Attr("href")
			if !exists || strings.Contains(href, "/browse/") {
				return
			}
			slug := ExtractSlugFromURL(href)
			if slug != "" && !seen[slug] {
				seen[slug] = true
				slugs = append(slugs, slug)
			}
		})
	}

	return slugs, nil
}

func ParseGameDetails(slug string, data []byte) (*domain.Game, []domain.Review, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}

	game := &domain.Game{
		Slug: slug,
	}

	// 1. Извлечение JSON-LD метаданных
	doc.Find("script[type='application/ld+json']").Each(func(i int, s *goquery.Selection) {
		raw := strings.TrimSpace(s.Text())
		if !gjson.Valid(raw) {
			return
		}
		parsed := gjson.Parse(raw)
		if parsed.Get("@type").String() == "VideoGame" || parsed.Get("name").Exists() {
			if game.Title == "" {
				game.Title = strings.TrimSpace(parsed.Get("name").String())
			}
			if game.Description == "" {
				game.Description = strings.TrimSpace(parsed.Get("description").String())
			}
			if game.CoverURL == "" {
				game.CoverURL = strings.TrimSpace(parsed.Get("image").String())
			}
			if game.Developer == "" {
				dev := parsed.Get("author.name").String()
				if dev == "" {
					dev = parsed.Get("publisher.name").String()
				}
				game.Developer = strings.TrimSpace(dev)
			}
			if game.VideoURL == "" {
				video := parsed.Get("trailer.embedUrl").String()
				if video == "" {
					video = parsed.Get("trailer.contentUrl").String()
				}
				game.VideoURL = strings.TrimSpace(video)
			}
			if game.ReleaseDate == "" {
				relDate := parsed.Get("datePublished").String()
				if relDate == "" {
					relDate = parsed.Get("dateCreated").String()
				}
				game.ReleaseDate = NormalizeReleaseDate(relDate)
			}
		}
	})

	// 2. Fallback из HTML DOM
	if game.Title == "" {
		game.Title = strings.TrimSpace(doc.Find("h1, [data-testid='hero-title']").First().Text())
	}
	if game.Description == "" {
		desc := doc.Find("[data-testid='hero-summary']").Text()
		desc = strings.TrimPrefix(strings.TrimSpace(desc), "Summary")
		game.Description = strings.TrimSpace(desc)
	}
	if game.Developer == "" {
		dev := doc.Find("[data-testid='hero-summary-developer']").Text()
		dev = strings.TrimPrefix(strings.TrimSpace(dev), "Developer:")
		game.Developer = strings.TrimSpace(dev)
	}
	if game.CoverURL == "" {
		imgSrc, _ := doc.Find("picture img[data-nuxt-img]").Attr("src")
		if imgSrc == "" {
			imgSrc, _ = doc.Find("img[data-testid='featured-trailer-poster']").Attr("src")
		}
		game.CoverURL = imgSrc
	}
	if game.VideoURL == "" {
		trailerEl := doc.Find("[data-testid='featured-trailer'] video, [data-testid='featured-trailer'] iframe")
		videoSrc, _ := trailerEl.Attr("src")
		game.VideoURL = videoSrc
	}
	if game.ReleaseDate == "" {
		dateText := doc.Find(".hero-release-date__value, [data-testid='hero-release-date'] .hero-release-date__value, .hero-release-date, .product-hero__release-date").First().Text()
		dateText = strings.TrimPrefix(strings.TrimSpace(dateText), "Released On:")
		game.ReleaseDate = NormalizeReleaseDate(strings.TrimSpace(dateText))
	}

	// 3. Извлечение Userscore для игры / дефолтной платформы
	var defaultUserScore *float64
	// Вариант 1: Поиск по блокам global-score-wrapper
	doc.Find("[data-testid='global-score-wrapper']").Each(func(i int, s *goquery.Selection) {
		header := strings.ToLower(s.Find("[data-testid='global-score-header']").Text())
		if strings.Contains(header, "user score") {
			valStr := strings.TrimSpace(s.Find("[data-testid='global-score-value']").Text())
			if valStr != "" && strings.ToLower(valStr) != "tbd" && strings.ToLower(valStr) != "null" {
				if val, err := strconv.ParseFloat(valStr, 64); err == nil {
					defaultUserScore = &val
				}
			}
		}
	})

	// Вариант 2 (fallback): Если wrapper не найден или структура изменилась, поиск от global-score-header вверх
	if defaultUserScore == nil {
		doc.Find("[data-testid='global-score-header']").Each(func(i int, s *goquery.Selection) {
			if defaultUserScore != nil {
				return
			}
			if strings.Contains(strings.ToLower(s.Text()), "user score") {
				wrapper := s.Closest("[data-testid='global-score-wrapper']")
				if wrapper.Length() == 0 {
					wrapper = s.ParentsFiltered(".flex").Last()
				}
				valStr := strings.TrimSpace(wrapper.Find("[data-testid='global-score-value']").Text())
				if valStr != "" && strings.ToLower(valStr) != "tbd" && strings.ToLower(valStr) != "null" {
					if val, err := strconv.ParseFloat(valStr, 64); err == nil {
						defaultUserScore = &val
					}
				}
			}
		})
	}

	// Вариант 3 (fallback): Поиск по атрибутам title/aria-label блока global-score
	if defaultUserScore == nil {
		doc.Find("[data-testid='global-score'] [title*='User score'], [data-testid='global-score'] [aria-label*='User score']").Each(func(i int, s *goquery.Selection) {
			if defaultUserScore != nil {
				return
			}
			valStr := strings.TrimSpace(s.Find("[data-testid='global-score-value']").Text())
			if valStr == "" {
				raw, exists := s.Attr("title")
				if !exists || raw == "" {
					raw, _ = s.Attr("aria-label")
				}
				parts := strings.Fields(raw)
				for idx, part := range parts {
					if strings.ToLower(part) == "score" && idx+1 < len(parts) {
						valStr = parts[idx+1]
						break
					}
				}
			}
			if valStr != "" && strings.ToLower(valStr) != "tbd" && strings.ToLower(valStr) != "null" {
				if val, err := strconv.ParseFloat(valStr, 64); err == nil {
					defaultUserScore = &val
				}
			}
		})
	}

	// 4. Платформы и Metascore
	platformMap := make(map[string]domain.GamePlatform)

	doc.Find("a[data-testid='product-score-card'], a.product-score-card").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists {
			return
		}

		platName := ""
		if u, err := url.Parse(href); err == nil {
			platName = u.Query().Get("platform")
		}
		if platName == "" {
			title, _ := s.Find(".game-platform-logo span[title]").Attr("title")
			platName = normalizePlatformName(title)
		}
		if platName == "" {
			return
		}

		// Извлечение оценки Metascore
		scoreText := strings.TrimSpace(s.Find(".c-siteReviewScore span, [data-testid='product-score-card'] span").Text())
		var metascore *int
		if scoreText != "" && strings.ToLower(scoreText) != "tbd" {
			if scoreVal, err := strconv.Atoi(scoreText); err == nil {
				metascore = &scoreVal
			}
		}

		platformMap[platName] = domain.GamePlatform{
			Platform:    platName,
			Metascore:   metascore,
			Userscore:   defaultUserScore,
			PlatformURL: href,
		}
	})

	// Если карточки не найдены, создаем дефолтную платформу "all"
	if len(platformMap) == 0 {
		platformMap["all"] = domain.GamePlatform{
			Platform:    "all",
			PlatformURL: "/game/" + slug,
			Userscore:   defaultUserScore,
		}
	}

	for _, p := range platformMap {
		game.Platforms = append(game.Platforms, p)
	}

	// 5. Парсинг отзывов
	var reviews []domain.Review

	// Отзывы критиков
	doc.Find("[data-testid='critic-reviews'] [data-testid='review-card'], .critic-reviews [data-testid='review-card']").Each(func(i int, s *goquery.Selection) {
		author := strings.TrimSpace(s.Find("[data-testid='review-card-header']").Text())
		text := strings.TrimSpace(s.Find("[data-testid='review-quote-text'], [data-testid='review-card-quote-block'], [data-testid='review-card-content']").Text())
		dateStr := strings.TrimSpace(s.Find("[data-testid='review-card-date']").Text())
		scoreText := strings.TrimSpace(s.Find(".c-siteReviewScore span").Text())

		var score *float64
		if scoreText != "" && strings.ToLower(scoreText) != "tbd" {
			if sc, err := strconv.ParseFloat(scoreText, 64); err == nil {
				score = &sc
			}
		}

		if author != "" || text != "" {
			reviews = append(reviews, domain.Review{
				ReviewType:  domain.ReviewTypeCritic,
				Author:      author,
				Score:       score,
				Text:        text,
				ContentHash: domain.ComputeContentHash(domain.ReviewTypeCritic, author, text),
				DateStr:     dateStr,
			})
		}
	})

	// Отзывы пользователей
	doc.Find("[data-testid='user-reviews'] [data-testid='review-card'], .user-reviews [data-testid='review-card']").Each(func(i int, s *goquery.Selection) {
		author := strings.TrimSpace(s.Find("[data-testid='review-card-header']").Text())
		text := strings.TrimSpace(s.Find("[data-testid='review-quote-text'], [data-testid='review-card-quote-block'], [data-testid='review-card-content']").Text())
		dateStr := strings.TrimSpace(s.Find("[data-testid='review-card-date']").Text())
		scoreText := strings.TrimSpace(s.Find(".c-siteReviewScore span").Text())

		var score *float64
		if scoreText != "" && strings.ToLower(scoreText) != "tbd" {
			if sc, err := strconv.ParseFloat(scoreText, 64); err == nil {
				score = &sc
			}
		}

		if author != "" || text != "" {
			reviews = append(reviews, domain.Review{
				ReviewType:  domain.ReviewTypeUser,
				Author:      author,
				Score:       score,
				Text:        text,
				ContentHash: domain.ComputeContentHash(domain.ReviewTypeUser, author, text),
				DateStr:     dateStr,
			})
		}
	})

	return game, reviews, nil
}

func normalizePlatformName(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	switch {
	case strings.Contains(raw, "playstation 5") || strings.Contains(raw, "ps5"):
		return "playstation-5"
	case strings.Contains(raw, "playstation 4") || strings.Contains(raw, "ps4"):
		return "playstation-4"
	case strings.Contains(raw, "xbox series") || strings.Contains(raw, "xbox-series-x"):
		return "xbox-series-x"
	case strings.Contains(raw, "xbox one"):
		return "xbox-one"
	case strings.Contains(raw, "switch 2"):
		return "nintendo-switch-2"
	case strings.Contains(raw, "switch"):
		return "nintendo-switch"
	case strings.Contains(raw, "pc"):
		return "pc"
	default:
		return strings.ReplaceAll(raw, " ", "-")
	}
}

// NormalizeReleaseDate приводит строки дат ("2022-02-25", "Feb 25, 2022", "February 25, 2022", "2022-02-25T00:00:00.000Z") к формату "YYYY-MM-DD".
func NormalizeReleaseDate(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	// Если уже в формате YYYY-MM-DD или ISO timestamp
	if len(raw) >= 10 {
		prefix := raw[:10]
		if _, err := time.Parse("2006-01-02", prefix); err == nil {
			return prefix
		}
	}

	formats := []string{
		"Jan 2, 2006",
		"January 2, 2006",
		"Jan 02, 2006",
		"January 02, 2006",
		"02 Jan 2006",
		"2 Jan 2006",
		"02 January 2006",
		"2 January 2006",
		"2006-01-02",
		time.RFC3339,
	}

	for _, layout := range formats {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.Format("2006-01-02")
		}
	}

	return raw
}
