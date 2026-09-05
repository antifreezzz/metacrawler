package scraper

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"

	"metacrawler/internal/domain"
)

type Client struct {
	httpClient tls_client.HttpClient
	baseURL    string
}

func NewClient() (*Client, error) {
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
		httpClient: client,
		baseURL:    "https://www.metacritic.com",
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
	return ParseGameDetails(slug, body)
}
