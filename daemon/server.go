//go:build windows

package daemon

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/jack-work/schedg"
)

var (
	orchardClientOnce sync.Once
	orchardClient     *http.Client
)

func orchardHTTPClient() *http.Client {
	orchardClientOnce.Do(func() {
		orchardClient = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
			Timeout: 0, // no timeout for SSE streams
		}
	})
	return orchardClient
}

// suppress unused import
var _ = io.Copy

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
	mux.HandleFunc("/api/rpc", s.handleRPC)
	mux.HandleFunc("/api/links", s.handleLinks)
	mux.HandleFunc("/api/orchard-token", s.handleOrchardToken)
	mux.HandleFunc("/api/orchard-login", s.handleOrchardLogin)
	mux.HandleFunc("/api/orchard/", s.handleOrchardProxy)
	mux.HandleFunc("/api/angls/events", s.handleAnglsSSE)
	mux.HandleFunc("/api/angls/", s.handleAnglAPIRoute)
	mux.HandleFunc("/api/angls", s.handleAnglsAPI)
	mux.HandleFunc("/api/queues/", s.handleQueueRoute)
	mux.HandleFunc("/api/queues", s.handleQueues)
	mux.HandleFunc("/api/completions", s.handleCompletions)
	mux.HandleFunc("/angls", s.handleDiscover)
	mux.HandleFunc("/angls/", s.handleAnglRoute)

	// Serve web UI if configured
	if webDir := s.daemon.config.Daemon.WebDir; webDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(webDir)))
	}

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

func (s *Server) handleAnglsAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.daemon.List())
}

func (s *Server) handleAnglAPIRoute(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, "/api/angls/")
	if suffix == "events" {
		return // handled by explicit registration
	}
	// Delegate to the existing /angls/ handler
	r.URL.Path = "/angls/" + suffix
	s.handleAnglRoute(w, r)
}

func (s *Server) handleAnglsSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	send := func() {
		data, _ := json.Marshal(s.daemon.List())
		w.Write([]byte("data: "))
		w.Write(data)
		w.Write([]byte("\n\n"))
		flusher.Flush()
	}

	send()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			send()
		}
	}
}

// ── Schedg queue endpoints ────────────────────────────────────

func (s *Server) handleQueues(w http.ResponseWriter, r *http.Request) {
	queues, err := schedg.ListQueues()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type entry struct {
		Name       string `json:"name"`
		Driver     string `json:"driver"`
		Path       string `json:"path"`
		Comparator string `json:"comparator"`
	}
	out := make([]entry, len(queues))
	for i, q := range queues {
		out[i] = entry{Name: q.Name, Driver: q.Driver, Path: q.Path, Comparator: q.Comparator}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

var (
	queueCacheMu sync.Mutex
	queueCache   = map[string]*schedg.DB{}
)

func getQueueSnapshot(name string) (schedg.QueueSnapshot, error) {
	queueCacheMu.Lock()
	defer queueCacheMu.Unlock()

	db, cached := queueCache[name]
	if !cached {
		var err error
		db, err = schedg.OpenByName(name)
		if err != nil {
			return schedg.QueueSnapshot{}, err
		}
		queueCache[name] = db
	} else {
		if err := db.Reload(); err != nil {
			db.Close()
			delete(queueCache, name)
			db2, err2 := schedg.OpenByName(name)
			if err2 != nil {
				return schedg.QueueSnapshot{}, err2
			}
			queueCache[name] = db2
			db = db2
		}
	}
	return db.Snapshot(), nil
}

func (s *Server) handleQueueRoute(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, "/api/queues/")
	parts := strings.SplitN(suffix, "/", 2)
	name := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch action {
	case "status":
		snap, err := getQueueSnapshot(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(enrichSnapshot(snap))

	case "events":
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		sendSnap := func() {
			snap, err := getQueueSnapshot(name)
			if err != nil {
				return
			}
			data, _ := json.Marshal(enrichSnapshot(snap))
			w.Write([]byte("data: "))
			w.Write(data)
			w.Write([]byte("\n\n"))
			flusher.Flush()
		}

		sendSnap()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				sendSnap()
			}
		}

	default:
		snap, err := getQueueSnapshot(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(enrichSnapshot(snap))
	}
}

type enrichedTask struct {
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	Description string            `json:"description,omitempty"`
	Priority    int64             `json:"priority"`
	Submitted   string            `json:"submitted,omitempty"`
	Attempts    int               `json:"attempts,omitempty"`
	Cancels     int               `json:"cancels,omitempty"`
	Reason      string            `json:"reason,omitempty"`
	LeasedAt    string            `json:"leasedAt,omitempty"`
	Caller      string            `json:"caller,omitempty"`
	KV          map[string]string `json:"kv,omitempty"`
}

func enrichSnapshot(snap schedg.QueueSnapshot) map[string]interface{} {
	enrich := func(tasks []schedg.Task) []enrichedTask {
		out := make([]enrichedTask, len(tasks))
		for i, t := range tasks {
			et := enrichedTask{
				ID: t.ID, Title: t.Title, Description: t.Description,
				Priority: t.Priority, KV: t.KV,
			}
			if !t.Submitted.IsZero() {
				et.Submitted = t.Submitted.Format(time.RFC3339)
			}
			if m, ok := snap.Meta[t.ID]; ok {
				et.Attempts = m.Attempts
				et.Cancels = m.Cancels
				et.Reason = m.Reason
				et.Caller = m.Caller
				if !m.LeasedAt.IsZero() {
					et.LeasedAt = m.LeasedAt.Format(time.RFC3339)
				}
			}
			out[i] = et
		}
		return out
	}

	return map[string]interface{}{
		"name":       snap.Name,
		"driver":     snap.Driver,
		"ready":      enrich(snap.Ready),
		"blocked":    enrich(snap.Blocked),
		"inflight":   enrich(snap.Inflight),
		"dead":       enrich(snap.Dead),
		"completed":  enrich(snap.Completed),
		"deps":       snap.Deps,
		"blockedBy":  snap.BlockedBy,
		"counts":     snap.Counts,
		"dbMeta":     snap.DBMeta,
		"snapshotAt": snap.SnapshotAt.Format(time.RFC3339),
	}
}

func (s *Server) handleCompletions(w http.ResponseWriter, r *http.Request) {
	ctx := r.URL.Query().Get("context")
	result := s.daemon.Completions(ctx)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (s *Server) handleLinks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.daemon.config.Links)
}

func (s *Server) handleOrchardToken(w http.ResponseWriter, r *http.Request) {
	token, err := s.daemon.tokens.Token()
	if err != nil {
		// If token acquisition needs auth, return a login message
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error(), "action": "login"})
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(token))
}

func (s *Server) handleOrchardLogin(w http.ResponseWriter, r *http.Request) {
	msg, err := s.daemon.tokens.ForceLogin()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

func (s *Server) handleOrchardProxy(w http.ResponseWriter, r *http.Request) {
	orchardURL := s.daemon.config.Orchard.URL
	if orchardURL == "" {
		http.Error(w, "orchard.url not configured", http.StatusServiceUnavailable)
		return
	}
	token, err := s.daemon.tokens.Token()
	if err != nil {
		http.Error(w, "token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Strip /api/orchard prefix, forward the rest
	targetPath := strings.TrimPrefix(r.URL.Path, "/api/orchard")
	targetURL := orchardURL + targetPath
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	proxyReq, _ := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	proxyReq.Header.Set("Authorization", "Bearer "+token)
	proxyReq.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	proxyReq.Header.Set("Accept", r.Header.Get("Accept"))

	// Use insecure client for self-signed local certs
	client := orchardHTTPClient()
	resp, err := client.Do(proxyReq)
	if err != nil {
		http.Error(w, "orchard: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy headers
	for k, v := range resp.Header {
		for _, vv := range v {
			w.Header().Add(k, vv)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Stream the response (important for SSE)
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
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
