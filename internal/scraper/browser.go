package scraper

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/zctx"
	"go.uber.org/zap"

	"github.com/ernado/lilith"
)

const (
	// defaultBrowserTimeout bounds a full Scrape (navigation + settle + capture).
	defaultBrowserTimeout = 45 * time.Second

	// defaultSettleTimeout bounds waiting for redirects and bot checks to clear.
	defaultSettleTimeout = 20 * time.Second

	// defaultChromeAddr is the remote allocator address used when none is given.
	// It matches the default chromedp/headless-shell debugging port.
	defaultChromeAddr = "http://127.0.0.1:9222"
)

// settleScript reports whether the page has finished loading and is past any
// interstitial bot check. It returns true once the document is complete, the
// title is not a known challenge page, and the body carries real text.
const settleScript = `(function () {
	if (document.readyState !== 'complete') return false;
	var title = (document.title || '').toLowerCase();
	var challenge = /just a moment|attention required|checking your browser|verifying you are human|access denied|ddos-guard|please wait/;
	if (challenge.test(title)) return false;
	var body = document.body ? (document.body.innerText || '').trim() : '';
	return body.length > 50;
})()`

var _ lilith.Scraper = (*Browser)(nil)

// Browser is a chromedp-backed scraper. It drives a remote headless Chrome so
// that JavaScript-rendered pages, client-side redirects and bot interstitials
// (e.g. Cloudflare "Just a moment...") resolve before content is extracted.
type Browser struct {
	allocCtx      context.Context
	cancel        context.CancelFunc
	timeout       time.Duration
	settleTimeout time.Duration
}

// BrowserOptions configures Browser construction.
type BrowserOptions struct {
	// Addr is the chromedp remote allocator address of an already-running
	// headless Chrome, e.g. "http://127.0.0.1:9222" or "ws://127.0.0.1:9222/".
	// Defaults to defaultChromeAddr. Proxying, sandboxing and other launch flags
	// are configured on that remote browser, not here.
	Addr string
	// Timeout bounds a single Scrape. Defaults to defaultBrowserTimeout.
	Timeout time.Duration
	// SettleTimeout bounds waiting for redirects/checks. Must be below Timeout.
	// Defaults to defaultSettleTimeout.
	SettleTimeout time.Duration
}

// NewBrowser connects to a remote headless Chrome via the chromedp remote
// allocator and returns a Browser scraper. Call Close to release the
// connection. The Chrome instance at options.Addr must already be running.
func NewBrowser(ctx context.Context, options BrowserOptions) (*Browser, error) {
	if options.Addr == "" {
		options.Addr = defaultChromeAddr
	}
	if options.Timeout <= 0 {
		options.Timeout = defaultBrowserTimeout
	}
	if options.SettleTimeout <= 0 {
		options.SettleTimeout = defaultSettleTimeout
	}
	if options.SettleTimeout >= options.Timeout {
		// Keep a margin so content can still be captured after a settle timeout.
		options.SettleTimeout = options.Timeout / 2
	}

	allocCtx, cancel := chromedp.NewRemoteAllocator(ctx, options.Addr)

	return &Browser{
		allocCtx:      allocCtx,
		cancel:        cancel,
		timeout:       options.Timeout,
		settleTimeout: options.SettleTimeout,
	}, nil
}

// Close releases the connection to the remote browser. The Browser must not be
// used afterwards.
func (b *Browser) Close() {
	b.cancel()
}

// Scrape navigates to url in a fresh tab, waits for redirects and bot checks to
// finish, then extracts the rendered page's content.
func (b *Browser) Scrape(ctx context.Context, rawURL string) (*lilith.ScrapeResult, error) {
	// A new tab off the shared browser. It derives from the background allocator
	// context, so propagate caller cancellation explicitly below.
	tabCtx, cancelTab := chromedp.NewContext(b.allocCtx)
	defer cancelTab()

	runCtx, cancel := context.WithTimeout(tabCtx, b.timeout)
	defer cancel()

	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-runCtx.Done():
		}
	}()

	// Record the HTTP status of document responses so the final page's status
	// can be reported. Keyed by URL because challenge flows load several.
	var (
		mu         sync.Mutex
		statuses   = map[string]int64{}
		lastStatus int64
	)
	chromedp.ListenTarget(runCtx, func(ev any) {
		if e, ok := ev.(*network.EventResponseReceived); ok &&
			e.Type == network.ResourceTypeDocument && e.Response != nil {
			mu.Lock()
			statuses[e.Response.URL] = e.Response.Status
			lastStatus = e.Response.Status
			mu.Unlock()
		}
	})

	if err := chromedp.Run(runCtx,
		network.Enable(),
		chromedp.Navigate(rawURL),
	); err != nil {
		return nil, errors.Wrap(err, "navigate")
	}

	// Wait for the page to settle. A settle timeout is not fatal: capture
	// whatever has rendered so far. WithPollingTimeout bounds the poll without
	// cancelling runCtx, leaving time for the capture below.
	var settled bool
	if err := chromedp.Run(runCtx,
		chromedp.Poll(settleScript, &settled, chromedp.WithPollingTimeout(b.settleTimeout)),
	); err != nil {
		zctx.From(ctx).Warn("Scraper page did not settle, capturing anyway",
			zap.String("url", rawURL),
			zap.Error(err),
		)
	}

	var (
		rendered string
		finalURL string
	)
	if err := chromedp.Run(runCtx,
		chromedp.Location(&finalURL),
		chromedp.OuterHTML("html", &rendered, chromedp.ByQuery),
	); err != nil {
		return nil, errors.Wrap(err, "capture html")
	}

	if finalURL == "" {
		finalURL = rawURL
	}

	mu.Lock()
	status := statuses[finalURL]
	if status == 0 {
		status = lastStatus
	}
	mu.Unlock()
	if status == 0 {
		// We have rendered HTML but never observed a document response status.
		status = 200
	}

	zctx.From(ctx).Info("Scraper rendered page",
		zap.String("url", finalURL),
		zap.Int64("status", status),
		zap.Bool("settled", settled),
		zap.Int("html_bytes", len(rendered)),
	)

	result := &lilith.ScrapeResult{
		URL:        finalURL,
		StatusCode: int(status),
	}

	// chromedp returns the live DOM serialized as UTF-8, so no charset decoding.
	if err := extractContent(strings.NewReader(rendered), result); err != nil {
		return nil, err
	}

	return result, nil
}
