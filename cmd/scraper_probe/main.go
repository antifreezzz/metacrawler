package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"metacrawler/internal/scraper"
)

func main() {
	client, err := scraper.NewClient()
	if err != nil {
		log.Fatalf("failed to create scraper client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Println("=== 1. Live Fetch New Releases ===")
	releases, err := client.FetchNewReleases(ctx)
	if err != nil {
		log.Fatalf("FetchNewReleases failed: %v", err)
	}
	fmt.Printf("Successfully fetched %d new releases! First 3: %v\n", len(releases), releases[:min(len(releases), 3)])

	fmt.Println("\n=== 2. Live Fetch Browse Page 1 ===")
	browseSlugs, err := client.FetchBrowsePage(ctx, 1)
	if err != nil {
		log.Fatalf("FetchBrowsePage failed: %v", err)
	}
	fmt.Printf("Successfully fetched %d games from browse page 1! First 3: %v\n", len(browseSlugs), browseSlugs[:min(len(browseSlugs), 3)])

	if len(releases) > 0 {
		targetSlug := releases[0]
		fmt.Printf("\n=== 3. Live Fetch Game Details for '%s' ===\n", targetSlug)
		game, reviews, err := client.FetchGameDetails(ctx, targetSlug)
		if err != nil {
			log.Fatalf("FetchGameDetails failed: %v", err)
		}
		data, _ := json.MarshalIndent(game, "", "  ")
		fmt.Printf("Game Details:\n%s\n", string(data))
		fmt.Printf("Reviews Count: %d\n", len(reviews))
		if len(reviews) > 0 {
			revSample, _ := json.MarshalIndent(reviews[0], "", "  ")
			fmt.Printf("Sample Review:\n%s\n", string(revSample))
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
