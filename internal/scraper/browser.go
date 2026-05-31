package scraper

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/zctx"
	"go.uber.org/zap"

	"github.com/ernado/lilith"
)

const (
	// defaultFlareSolverrAddr is the FlareSolverr endpoint used when none is given.
	defaultFlareSolverrAddr = "http://127.0.0.1:8191/v1"

	// defaultTimeout bounds a single Scrape call end-to-end.
	defaultTimeout = 60 * time.Second
)

var _ lilith.Scraper = (*FlareSolverr)(nil)

// FlareSolverr is a scraper backed by a FlareSolverr instance. It delegates
// page fetching to FlareSolverr, which handles JavaScript rendering, Cloudflare
// challenges and similar bot-protection mechanisms.
type FlareSolverr struct {
	addr    string
	timeout time.Duration
	client  *http.Client
}

// FlareSolverrOptions configures FlareSolverr construction.
type FlareSolverrOptions struct {
	// Addr is the FlareSolverr API base URL, e.g. "http://127.0.0.1:8191/v1".
	// Defaults to defaultFlareSolverrAddr.
	Addr string
	// Timeout bounds a single Scrape. Defaults to defaultTimeout.
	Timeout time.Duration
}

// NewFlareSolverr returns a FlareSolverr scraper pointed at the given endpoint.
func NewFlareSolverr(options FlareSolverrOptions) *FlareSolverr {
	if options.Addr == "" {
		options.Addr = defaultFlareSolverrAddr
	}
	if options.Timeout <= 0 {
		options.Timeout = defaultTimeout
	}

	return &FlareSolverr{
		addr:    options.Addr,
		timeout: options.Timeout,
		client:  &http.Client{Timeout: options.Timeout + 5*time.Second},
	}
}

// flareSolverrRequest is the JSON body sent to the FlareSolverr API.
type flareSolverrRequest struct {
	Cmd        string `json:"cmd"`
	URL        string `json:"url"`
	MaxTimeout int    `json:"maxTimeout"`
}

// flareSolverrResponse is the JSON body returned by the FlareSolverr API.
type flareSolverrResponse struct {
	Status   string `json:"status"`
	Message  string `json:"message"`
	Solution struct {
		URL      string `json:"url"`
		Status   int    `json:"status"`
		Response string `json:"response"`
	} `json:"solution"`
}

// Scrape fetches the page at rawURL via FlareSolverr and extracts its content.
func (f *FlareSolverr) Scrape(ctx context.Context, rawURL string) (*lilith.ScrapeResult, error) {
	reqBody := flareSolverrRequest{
		Cmd:        "request.get",
		URL:        rawURL,
		MaxTimeout: int(f.timeout.Milliseconds()),
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, errors.Wrap(err, "marshal request")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.addr, bytes.NewReader(body))
	if err != nil {
		return nil, errors.Wrap(err, "build request")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "do request")
	}
	defer resp.Body.Close()

	var fsResp flareSolverrResponse
	if err := json.NewDecoder(resp.Body).Decode(&fsResp); err != nil {
		return nil, errors.Wrap(err, "decode response")
	}

	if fsResp.Status != "ok" {
		return nil, errors.Errorf("flaresolverr error: %s", fsResp.Message)
	}

	finalURL := fsResp.Solution.URL
	if finalURL == "" {
		finalURL = rawURL
	}

	statusCode := fsResp.Solution.Status
	if statusCode == 0 {
		statusCode = 200
	}

	zctx.From(ctx).Info("FlareSolverr fetched page",
		zap.String("url", finalURL),
		zap.Int("status", statusCode),
		zap.Int("html_bytes", len(fsResp.Solution.Response)),
	)

	result := &lilith.ScrapeResult{
		URL:        finalURL,
		StatusCode: statusCode,
	}

	if err := extractContent(strings.NewReader(fsResp.Solution.Response), result); err != nil {
		return nil, err
	}

	return result, nil
}
