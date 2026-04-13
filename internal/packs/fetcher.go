package packs

// fetcher.go owns the Fetcher interface and its production HTTP
// implementation. Split out from download.go as of #101 so the
// interface stays live even while the per-artifact download flow is
// disabled — callers that need to pull an asset by URL (and any future
// caller re-wiring from #101's revival path) have a stable seam.

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// Fetcher is the surface HTTP-asset-pulling code uses to read bytes
// from a release URL. The production implementation is httpFetcher;
// tests substitute their own to avoid hitting the real internet.
type Fetcher interface {
	// Get returns a reader for the body at url. The reader's Close
	// must be called by the caller. A non-2xx status returns an error.
	Get(ctx context.Context, url string) (io.ReadCloser, error)
}

// FetcherFunc is the http.HandlerFunc-style adapter for Fetcher: it
// lets a plain function be used wherever a Fetcher is required.
type FetcherFunc func(ctx context.Context, url string) (io.ReadCloser, error)

// Get satisfies the Fetcher interface by calling the underlying
// function.
func (f FetcherFunc) Get(ctx context.Context, url string) (io.ReadCloser, error) {
	return f(ctx, url)
}

// httpFetcher is the production Fetcher implementation. It's a thin
// wrapper around an http.Client.
type httpFetcher struct {
	client *http.Client
}

// NewHTTPFetcher returns a production Fetcher backed by the supplied
// client. Pass http.DefaultClient unless you specifically need a
// non-default timeout or transport.
func NewHTTPFetcher(client *http.Client) Fetcher {
	if client == nil {
		client = http.DefaultClient
	}
	return &httpFetcher{client: client}
}

func (h *httpFetcher) Get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("new request %s: %w", url, err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}
	return resp.Body, nil
}
