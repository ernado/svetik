// Package static implements a temporary in-memory file server.
package static

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ernado/lilith"
	"github.com/go-faster/errors"
	"github.com/google/uuid"
)

var _ lilith.FileStore = (*Server)(nil)

// Server is a temporary in-memory file server.
// Files are stored in memory and served over HTTP.
type Server struct {
	addr    string
	baseURL string

	mu    sync.RWMutex
	files map[string][]byte
}

// New creates a new Server that listens on addr and uses baseURL as the URL prefix.
func New(addr, baseURL string) *Server {
	return &Server{
		addr:    addr,
		baseURL: strings.TrimRight(baseURL, "/"),
		files:   make(map[string][]byte),
	}
}

// Upload reads all data from r, stores it under a new UUID, and returns the
// public URL for the uploaded file.
func (s *Server) Upload(r io.Reader) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", errors.Wrap(err, "read")
	}

	id := uuid.New().String()

	s.mu.Lock()
	s.files[id] = data
	s.mu.Unlock()

	return s.baseURL + "/" + id, nil
}

// Run starts the HTTP server and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.serveFile)

	srv := &http.Server{
		Addr:    s.addr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.Wrap(err, "listen and serve")
	}

	return nil
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/")

	s.mu.RLock()
	data, ok := s.files[id]
	s.mu.RUnlock()

	if !ok {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeContent(w, r, id, time.Time{}, bytes.NewReader(data))
}
