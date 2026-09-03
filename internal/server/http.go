// Package server assembles the single HTTP listener: health endpoints, the
// REST API and the MCP streamable-HTTP endpoint. There is no authentication
// here on purpose: on the platform the agentgateway JWT policy in front of the
// route and muster in front of the MCP endpoint are the trust boundary.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/api"
)

// Config configures the listener.
type Config struct {
	Addr       string
	MCPEnabled bool
	MCPPath    string
}

// Server is the assembled HTTP server.
type Server struct {
	http *http.Server
	log  *slog.Logger
}

// New builds the server.
func New(cfg Config, svc *agents.Service, mcpSrv *mcpserver.MCPServer, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.MCPPath == "" {
		cfg.MCPPath = "/mcp"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Readiness does not track the API server or the chart registry: the API
	// must stay reachable so clients read the failure from the response
	// (a 403 on a write, get_info's chart.error) instead of an unready
	// Service.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	api.NewREST(svc, log).Register(mux)

	s := &Server{log: log}
	if cfg.MCPEnabled && mcpSrv != nil {
		mux.Handle(cfg.MCPPath, mcpserver.NewStreamableHTTPServer(mcpSrv, mcpserver.WithEndpointPath(cfg.MCPPath)))
	}
	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: MCP streams outlive any fixed value.
		IdleTimeout: 120 * time.Second,
	}
	return s, nil
}

// Handler exposes the mux (tests).
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Run serves until ctx is done, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("listening", "addr", s.http.Addr)
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	s.log.Info("server stopped")
	return nil
}
