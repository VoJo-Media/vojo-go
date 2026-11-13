# Vojo Go library

The official Go client for the [Vojo](https://vojo-media.com) API: proof that your ads actually played on digital out-of-home screens.

> **Pre-1.0 and not yet tagged.** Expect changes before 1.0.

## Install

```bash
go get github.com/VoJo-Media/vojo-go
```

Requires Go 1.23 or newer, for range-over-func iterators. Standard library only.

## First call

An organisation owner creates keys in the Vojo portal. A live key starts with `vojo_live_` and comes from **Developers → API keys**. A test key starts with `vojo_test_` and comes from **Developers → Sandboxes**: it reaches only its own sandbox, with the same base URL and endpoints. Objects returned with a test key have `livemode` set to `false` (`screen.Livemode`). See [Authentication](https://developers.vojo-media.com/getting-started/authentication) and [Test in a sandbox](https://developers.vojo-media.com/guides/test-in-a-sandbox) on https://developers.vojo-media.com.

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/VoJo-Media/vojo-go"
)

func main() {
	client, err := vojo.NewClient("vojo_live_...") // or "" to read VOJO_SECRET_KEY
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	for screen, err := range client.Screens.List(ctx, nil).All() {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(screen.ID, screen.Name)
	}
}
```

`All` walks every item across pages, fetching as it goes.

## Common tasks

```go
// One object
screen, err := client.Screens.Retrieve(ctx, "scr_123")
if err != nil {
	log.Fatal(err)
}
fmt.Println(screen.Name)

// Create. A retry can never create it twice: every attempt carries one Idempotency-Key.
site, err := client.Sites.Create(ctx, &vojo.CreateSiteRequest{Name: "Kings Cross"})
site, err = client.Sites.Create(ctx, &vojo.CreateSiteRequest{Name: "Kings Cross"},
	vojo.WithIdempotencyKey("my-own-key-123")) // on the call: NewClient refuses it
fmt.Println(site.ID)

// Update. A nil field is unchanged; vojo.Null sends null.
_, err = client.Screens.Update(ctx, "scr_123", &vojo.UpdateScreenRequest{
	Name:  vojo.Ptr("Platform 1"),
	Notes: vojo.Null[string](),
})

// Sub-resources follow the path
err = client.WebhookEndpoints.Deliveries.Retry(ctx, "whe_123", "del_123")

// One page at a time. Limit is the page size, at most 100.
page, err := client.Plays.List(ctx, &vojo.PlaysListQuery{ScreenID: "scr_123", Limit: 100}).FirstPage()
fmt.Println(len(page.Data), page.HasMore())

// Statistics keep their meta
stats, err := client.Stats.Plays.Retrieve(ctx, &vojo.StatsPlaysRetrieveQuery{Range: "7d"})
fmt.Println(stats.Data, string(stats.Meta))
```

Some operations are not served yet during early access. Their doc comments say so, and calling one answers with a 404 whose message says the same.

## Errors

Errors work with `errors.Is` and `errors.As`.

```go
_, err := client.Screens.Retrieve(ctx, "scr_missing")

var apiErr *vojo.APIError
switch {
case errors.Is(err, vojo.ErrNotFound):
	// handle a missing screen
case errors.Is(err, vojo.ErrRateLimit):
	errors.As(err, &apiErr)
	wait, _ := apiErr.RetryAfter()
	fmt.Println("wait", wait)
case errors.As(err, &apiErr):
	fmt.Println(apiErr.Status, apiErr.Code, apiErr.RequestID, apiErr.Problem)
}
```

| Sentinel | When |
| --- | --- |
| `ErrAuthentication` | 401 |
| `ErrPermission` | 402 (plan limit: see `Problem`) and 403 |
| `ErrNotFound` | 404 |
| `ErrValidation` | 400, 413, 422 (field reasons in `Errors`) |
| `ErrConflict` | 409 |
| `ErrRateLimit` | 429 (`RetryAfter()`) |
| `ErrServer` | 5xx |
| `ErrPagination` | The API returned paging data that cannot be followed safely |

A request with no response gives a `*vojo.ConnectionError`, with `Timeout` set when it timed out. It unwraps to the cause, such as `context.Canceled`.

Codes can be added within v1, so always handle a code you have not seen.

## Webhooks

Pass the raw body, exactly as received. No API key is needed.

```go
import "github.com/VoJo-Media/vojo-go/webhook"

func handler(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "unreadable", http.StatusBadRequest)
		return
	}
	event, err := webhook.ConstructEvent(body, r.Header.Get(webhook.SignatureHeader), signingSecret)
	if err != nil {
		http.Error(w, "bad signature", http.StatusBadRequest)
		return
	}

	// Dedupe on event.ID: it stays the same across retries.
	if event.Type == webhook.TypeVerificationScore {
		var score webhook.VerificationScoreEvent
		if err := event.Decode(&score); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
```

While you roll a secret, hold both: `webhook.ConstructEvent(body, header, newSecret, webhook.WithAdditionalSecret(oldSecret))`.

`errors.As` with a `*webhook.VerificationError` gives the reason in `Kind`. To test your own handler, sign a payload with `webhook.GenerateTestHeader(body, secret, time.Now())`.

## Configuration

```go
client, err := vojo.NewClient("vojo_live_...",
	vojo.WithOrganisation("org_123"), // OAuth access tokens must set this; secret keys do not need it
	vojo.WithVersion("2026-10-07"),   // defaults to the version this release was built against
	vojo.WithTimeout(30*time.Second), // per attempt, default 60s; use the context for an overall limit
	vojo.WithMaxRetries(5),           // default 2
)
if err != nil {
	log.Fatal(err)
}

// The same options work on one call
screen, err := client.Screens.Retrieve(ctx, "scr_123", vojo.WithTimeout(5*time.Second))
fmt.Println(screen.Name)
```

`vojo.WithTokenSource` reads the token on each request, for OAuth tokens that refresh.

Failed requests are retried only when that is safe: connection failures, 429 and 5xx on operations that cannot apply twice. A `Retry-After` is honoured, up to 60 seconds; beyond that you get the error straight away.

Only a 2xx counts as success. Redirects are not followed, even on a client from `WithHTTPClient`: the API does not send them, so a 3xx is an `*APIError` and usually means the base URL is wrong.

Cancelling the context stops a call at once, even during a wait between retries.

## Response details

Results carry the HTTP details on `LastResponse`: `Status`, `Header`, `RequestID`, `Meta` and `IdempotentReplayed`.

```go
wrapper, err := client.Wrappers.Create(ctx, &vojo.CreateWrapperRequest{ScreenID: vojo.Ptr("scr_123")})
if err != nil {
	log.Fatal(err)
}
fmt.Println(wrapper.LastResponse.RequestID, string(wrapper.LastResponse.Meta))
```

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md).
