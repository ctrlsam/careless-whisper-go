package metrics

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ctrlsam/careless-whisper-go/internal/model"
)

// Server exposes the latest analysis snapshot as Prometheus text at /metrics.
type Server struct {
	phoneNumber string

	mu       sync.RWMutex
	analysis model.Analysis
	hasData  bool

	httpServer *http.Server
}

// NewServer returns a metrics server labelled with phoneNumber.
func NewServer(phoneNumber string) *Server {
	return &Server{phoneNumber: phoneNumber}
}

// Update stores the latest analysis snapshot.
func (s *Server) Update(analysis model.Analysis) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.analysis = analysis
	s.hasData = true
}

// Start begins serving on the given port. It returns once the listener is open.
func (s *Server) Start(port int) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", s.handleMetrics)
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	s.httpServer = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = s.httpServer.Serve(listener) }()
	return nil
}

// Stop shuts the server down.
func (s *Server) Stop(ctx context.Context) error {
	if s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	analysis := s.analysis
	hasData := s.hasData
	s.mu.RUnlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	if !hasData {
		return
	}
	_ = WritePrometheus(w, s.phoneNumber, analysis)
}
