// A first tour of the client. Set VOJO_SECRET_KEY, and VOJO_BASE_URL to use a mock.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/VoJo-Media/vojo-go"
)

func main() {
	ctx := context.Background()
	opts := []vojo.Option{}
	if base := os.Getenv("VOJO_BASE_URL"); base != "" {
		opts = append(opts, vojo.WithBaseURL(base))
	}
	client, err := vojo.NewClient("", opts...)
	if err != nil {
		log.Fatal(err)
	}

	page, err := client.Screens.List(ctx, &vojo.ScreensListQuery{Limit: 10}).FirstPage()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d screens on the first page, more: %t\n", len(page.Data), page.HasMore())

	if len(page.Data) > 0 {
		screen, err := client.Screens.Retrieve(ctx, page.Data[0].ID)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(screen.ID, screen.Name)
	}

	site, err := client.Sites.Create(ctx, &vojo.CreateSiteRequest{Name: "Kings Cross"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("created", site.ID, "replayed:", site.LastResponse.IdempotentReplayed)

	stats, err := client.Stats.Verification.Retrieve(ctx, &vojo.StatsVerificationRetrieveQuery{Type: "wrapper"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("meta", string(stats.Meta))

	for _, err := range client.Screens.List(ctx, nil).All() {
		if errors.Is(err, vojo.ErrPagination) {
			// Only a mock does this: it replays one page with the same next_cursor.
			fmt.Println("paging guard:", err)
		} else if err != nil {
			log.Fatal(err)
		}
	}
}
