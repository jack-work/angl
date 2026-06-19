//go:build windows

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
)

const pipeName = `\\.\pipe\angld`

type Server struct {
	daemon *Daemon
}

func NewServer(d *Daemon) *Server {
	return &Server{daemon: d}
}

func (s *Server) Run(ctx context.Context, port int) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/rpc", s.handleRPC)
	mux.HandleFunc("/api/links", s.handleLinks)
	mux.HandleFunc("/angls", s.handleDiscover)
	mux.HandleFunc("/angls/", s.handleAnglRoute)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	// Named pipe for local JSON-RPC.
	go s.servePipe(ctx)

	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	s.daemon.logger.Printf("http :%d | pipe %s", port, pipeName)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// --- HTTP handlers ---

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req RPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rpcErr(nil, -32700, "parse error"))
		return
	}
	resp := s.daemon.HandleRPC(req)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleLinks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.daemon.config.Links)
}

func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.daemon.List())
}

func (s *Server) handleAnglRoute(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/angls/")
	parts := strings.SplitN(path, "/", 2)
	name := parts[0]
	if name == "" {
		http.NotFound(w, r)
		return
	}

	// /angls/<name> -- status
	if len(parts) == 1 {
		status, err := s.daemon.StatusOf(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
		return
	}

	// /angls/<name>/tail -- SSE stream
	if parts[1] == "tail" {
		s.handleTail(w, r, name)
		return
	}

	http.NotFound(w, r)
}

func (s *Server) handleTail(w http.ResponseWriter, r *http.Request, name string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	ring, err := s.daemon.TailOutput(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Replay history.
	historyCount := 100
	if h := r.URL.Query().Get("history"); h != "" {
		fmt.Sscanf(h, "%d", &historyCount)
	}
	for _, line := range ring.History(historyCount) {
		fmt.Fprintf(w, "data: %s\n\n", line)
	}
	flusher.Flush()

	// Stream new lines.
	id, ch := ring.Subscribe()
	defer ring.Unsubscribe(id)

	for {
		select {
		case line, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", line)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// --- Named pipe server ---

func (s *Server) servePipe(ctx context.Context) {
	l, err := winio.ListenPipe(pipeName, &winio.PipeConfig{
		InputBufferSize:  4096,
		OutputBufferSize: 65536,
	})
	if err != nil {
		s.daemon.logger.Printf("warning: pipe unavailable: %v", err)
		return
	}

	go func() {
		<-ctx.Done()
		l.Close()
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		go s.handlePipeConn(conn)
	}
}

func (s *Server) handlePipeConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Minute))

	var req RPCRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		json.NewEncoder(conn).Encode(rpcErr(nil, -32700, "parse error"))
		return
	}
	resp := s.daemon.HandleRPC(req)
	json.NewEncoder(conn).Encode(resp)
}

// IsDaemonRunning checks if another daemon instance is already listening.
func IsDaemonRunning() bool {
	timeout := 2 * time.Second
	conn, err := winio.DialPipe(pipeName, &timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
