package scraper

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"

	"metacrawler/internal/domain"
)

type Client struct {
	httpClient tls_client.HttpClient
	baseURL    string
	// interRequestDelay - пауза между запросами страниц одной игры (главная + подстраницы отзывов),
	// чтобы не создавать пачку одновременных обращений к Metacritic.
	interRequestDelay time.Duration
}

func NewClient() (*Client, error) {
	return newClient("https://www.metacritic.com", 1200*time.Millisecond)
}

// NewClientWithBaseURL создает клиент на заданный базовый URL без межзапросной паузы (для тестов).
func NewClientWithBaseURL(baseURL string) (*Client, error) {
	return newClient(baseURL, 0)
}

func newClient(baseURL string, interRequestDelay time.Duration) (*Client, error) {
	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(20),
		tls_client.WithClientProfile(profiles.Chrome_133),
		tls_client.WithNotFollowRedirects(),
	}

	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
	if err != nil {
		return nil, fmt.Errorf("create tls client: %w", err)
	}

	return &Client{
		httpClient:        client,
		baseURL:           baseURL,
		interRequestDelay: interRequestDelay,
	}, nil
}

func (c *Client) get(ctx context.Context, targetURL string) ([]byte, error) {
	req, err := fhttp.NewRequestWithContext(ctx, "GET", targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Sec-Ch-Ua", `"Not(A:Brand";v="99", "Google Chrome";v="133", "Chromium";v="133"`)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", `"Linux"`)
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Upgrade-Insecure-Requests", "1")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, targetURL)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	return body, nil
}

func (c *Client) FetchNewReleases(ctx context.Context) ([]string, error) {
	targetURL := fmt.Sprintf("%s/game/", c.baseURL)
	body, err := c.get(ctx, targetURL)
	if err != nil {
		return nil, err
	}
	return ParseNewReleases(body)
}

func (c *Client) FetchBrowsePage(ctx context.Context, page int) ([]string, error) {
	if page < 1 {
		page = 1
	}
	targetURL := fmt.Sprintf("%s/browse/game/all/all/all-time/new/?page=%d", c.baseURL, page)
	body, err := c.get(ctx, targetURL)
	if err != nil {
		return nil, err
	}
	return ParseBrowseCatalog(body)
}

func (c *Client) FetchGameDetails(ctx context.Context, slug string) (*domain.Game, []domain.Review, error) {
	slug = strings.Trim(slug, "/")
	targetURL := fmt.Sprintf("%s/game/%s/", c.baseURL, slug)
	body, err := c.get(ctx, targetURL)
	if err != nil {
		return nil, nil, err
	}
	game, reviews, err := ParseGameDetails(slug, body)
	if err != nil {
		return nil, nil, err
	}

	// Userscore принадлежит конкретной платформе: тянем его со страницы
	// user-reviews этой платформы, а не копируем значение главной страницы.
	for i := range game.Platforms {
		if game.Platforms[i].Platform == "" {
			continue
		}
		if score, scoreErr := c.FetchPlatformUserScore(ctx, slug, game.Platforms[i].Platform); scoreErr == nil && score != nil {
			game.Platforms[i].Userscore = score
		}
	}

	// Полные списки отзывов живут на подстраницах /critic-reviews/ и /user-reviews/.
	// Главная страница содержит лишь несколько карточек-цитат.
	reviews = append(reviews, c.fetchReviewSubpage(ctx, slug, "critic-reviews", domain.ReviewTypeCritic)...)
	reviews = append(reviews, c.fetchReviewSubpage(ctx, slug, "user-reviews", domain.ReviewTypeUser)...)

	return game, reviews, nil
}

// FetchPlatformUserScore получает userscore одной платформы с её страницы
// /game/{slug}/user-reviews/?platform={platform}. Ошибка или "tbd" означает
// отсутствие честных данных, а не повод выдумать значение.
func (c *Client) FetchPlatformUserScore(ctx context.Context, slug, platform string) (*float64, error) {
	if platform == "" {
		return nil, fmt.Errorf("empty platform")
	}
	if c.interRequestDelay > 0 {
		select {
		case <-time.After(c.interRequestDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	targetURL := fmt.Sprintf("%s/game/%s/user-reviews/?platform=%s", c.baseURL, slug, url.QueryEscape(platform))
	body, err := c.get(ctx, targetURL)
	if err != nil {
		return nil, err
	}
	score := ParseUserScore(body)
	if score == nil {
		return nil, fmt.Errorf("no user score for platform %q", platform)
	}
	return score, nil
}

// fetchReviewSubpage загружает и парсит одну подстраницу отзывов.
// Ошибка загрузки не фатальна: возвращаем пустой список, основной сбор продолжается.
func (c *Client) fetchReviewSubpage(ctx context.Context, slug, subpage string, reviewType domain.ReviewType) []domain.Review {
	if c.interRequestDelay > 0 {
		select {
		case <-time.After(c.interRequestDelay):
		case <-ctx.Done():
			return nil
		}
	}

	targetURL := fmt.Sprintf("%s/game/%s/%s/", c.baseURL, slug, subpage)
	body, err := c.get(ctx, targetURL)
	if err != nil {
		return nil
	}
	return ParseReviewSubpage(body, reviewType)
}
