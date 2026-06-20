// angl-web: unified web UI for angl daemon + schedg queues.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"bytes"
	"io"
	"sort"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jack-work/schedg"
)

func main() {
	addr := flag.String("addr", ":4343", "listen address")
	webDir := flag.String("web", "web/dist", "path to frontend build output")
	daemonURL := flag.String("daemon", "http://localhost:3333", "angl daemon HTTP base URL")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	srv := &webServer{daemon: *daemonURL}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/angls/events", srv.handleAnglsSSE)
	mux.HandleFunc("/api/angls/", srv.handleAnglRoute)
	mux.HandleFunc("/api/angls", srv.handleAngls)
	mux.HandleFunc("/api/queues", srv.handleQueues)
	mux.HandleFunc("/api/queues/", srv.handleQueueRoute)
	mux.HandleFunc("/api/completions", srv.handleCompletions)
	mux.HandleFunc("/api/rpc", srv.handleRPC)
	mux.HandleFunc("/api/orchard-token", handleOrchardToken)
	mux.Handle("/", http.FileServer(http.Dir(*webDir)))

	httpSrv := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		<-ctx.Done()
		httpSrv.Shutdown(context.Background())
	}()

	queues, _ := schedg.ListQueues()
	log.Printf("angl-web listening on %s (daemon: %s, schedg queues: %d)", *addr, *daemonURL, len(queues))
	if err := httpSrv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

type webServer struct {
	daemon string
}

// ── Angl proxy ──────────────────────────────────────────────────

func (s *webServer) proxyJSON(w http.ResponseWriter, path string) {
	resp, err := http.Get(s.daemon + path)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (s *webServer) handleAngls(w http.ResponseWriter, r *http.Request) {
	s.proxyJSON(w, "/angls")
}

func (s *webServer) handleAnglRoute(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, "/api/angls/")
	if suffix == "events" {
		return
	}
	path := "/angls/" + suffix
	parts := strings.SplitN(suffix, "/", 2)
	if len(parts) == 2 && parts[1] == "tail" {
		s.proxySSE(w, r, path+"?"+r.URL.RawQuery)
		return
	}
	s.proxyJSON(w, path)
}

func (s *webServer) proxySSE(w http.ResponseWriter, r *http.Request, path string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), "GET", s.daemon+path, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{Timeout: 0}).Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			flusher.Flush()
		}
		if err != nil {
			return
		}
	}
}

func (s *webServer) handleAnglsSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	send := func() {
		resp, err := http.Get(s.daemon + "/angls")
		if err != nil {
			return
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
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

// ── Schedg ──────────────────────────────────────────────────────

func (s *webServer) handleQueues(w http.ResponseWriter, r *http.Request) {
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

func (s *webServer) handleQueueRoute(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, "/api/queues/")
	parts := strings.SplitN(suffix, "/", 2)
	name := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	switch action {
	case "status":
		snap, err := getSnapshot(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(snap)
	case "events":
		s.handleQueueSSE(w, r, name)
	default:
		http.NotFound(w, r)
	}
}

func (s *webServer) handleQueueSSE(w http.ResponseWriter, r *http.Request, name string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	send := func() {
		snap, err := getSnapshot(name)
		if err != nil {
			return
		}
		data, _ := json.Marshal(snap)
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

var (
	dbMu    sync.Mutex
	dbCache = map[string]*schedg.DB{}
)

type apiSnapshot struct {
	Name       string              `json:"name"`
	Driver     string              `json:"driver"`
	Ready      []apiTask           `json:"ready"`
	Blocked    []apiTask           `json:"blocked"`
	Inflight   []apiTask           `json:"inflight"`
	Dead       []apiTask           `json:"dead"`
	Completed  []apiTask           `json:"completed"`
	Deps       []apiDep            `json:"deps"`
	BlockedBy  map[string][]string `json:"blockedBy"`
	Counts     map[string]int      `json:"counts"`
	DBMeta     map[string]string   `json:"dbMeta,omitempty"`
	SnapshotAt string              `json:"snapshotAt"`
}

type apiTask struct {
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

type apiDep struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func convertTask(t schedg.Task, meta map[string]schedg.TaskMeta) apiTask {
	at := apiTask{
		ID: t.ID, Title: t.Title, Description: t.Description,
		Priority: t.Priority, KV: t.KV,
	}
	if !t.Submitted.IsZero() {
		at.Submitted = t.Submitted.UTC().Format(time.RFC3339)
	}
	if m, ok := meta[t.ID]; ok {
		at.Attempts = m.Attempts
		at.Cancels = m.Cancels
		at.Reason = m.Reason
		at.Caller = m.Caller
		if !m.LeasedAt.IsZero() {
			at.LeasedAt = m.LeasedAt.UTC().Format(time.RFC3339)
		}
	}
	return at
}

func convertSnap(name string, s schedg.QueueSnapshot) *apiSnapshot {
	a := &apiSnapshot{
		Name: name, Driver: "",
		BlockedBy: s.BlockedBy, Counts: s.Counts,
		DBMeta: s.DBMeta,
		SnapshotAt: s.SnapshotAt.UTC().Format(time.RFC3339),
	}
	for _, t := range s.Ready { a.Ready = append(a.Ready, convertTask(t, s.Meta)) }
	for _, t := range s.Blocked { a.Blocked = append(a.Blocked, convertTask(t, s.Meta)) }
	for _, t := range s.Inflight { a.Inflight = append(a.Inflight, convertTask(t, s.Meta)) }
	for _, t := range s.Dead { a.Dead = append(a.Dead, convertTask(t, s.Meta)) }
	for _, t := range s.Completed { a.Completed = append(a.Completed, convertTask(t, s.Meta)) }
	for _, d := range s.Deps { a.Deps = append(a.Deps, apiDep{From: d.From, To: d.To}) }
	if a.Ready == nil { a.Ready = []apiTask{} }
	if a.Blocked == nil { a.Blocked = []apiTask{} }
	if a.Inflight == nil { a.Inflight = []apiTask{} }
	if a.Dead == nil { a.Dead = []apiTask{} }
	if a.Completed == nil { a.Completed = []apiTask{} }
	if a.Deps == nil { a.Deps = []apiDep{} }
	return a
}

func getSnapshot(name string) (*apiSnapshot, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	db, cached := dbCache[name]
	if !cached {
		var err error
		db, err = schedg.OpenByName(name)
		if err != nil {
			return nil, err
		}
		dbCache[name] = db
	} else {
		if err := db.Reload(); err != nil {
			db.Close()
			delete(dbCache, name)
			var err2 error
			db, err2 = schedg.OpenByName(name)
			if err2 != nil {
				return nil, err2
			}
			dbCache[name] = db
		}
	}

	snap := db.Snapshot()
	return convertSnap(name, snap), nil
}

func (s *webServer) handleCompletions(w http.ResponseWriter, r *http.Request) {
	ctx := r.URL.Query().Get("context")
	body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"completions","params":{"context":"%s"},"id":1}`, ctx)
	resp, err := http.Post(s.daemon+"/rpc", "application/json", bytes.NewBufferString(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	// Extract result from RPC envelope
	var envelope struct{ Result json.RawMessage `json:"result"` }
	json.Unmarshal(raw, &envelope)
	w.Header().Set("Content-Type", "application/json")
	if envelope.Result != nil {
		w.Write(envelope.Result)
	} else {
		w.Write(raw)
	}
}

func (s *webServer) handleRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	s.proxyJSONBody(w, "/rpc", body)
}

func (s *webServer) proxyJSONBody(w http.ResponseWriter, path string, body []byte) {
	resp, err := http.Post(s.daemon+path, "application/json", bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func handleOrchardToken(w http.ResponseWriter, r *http.Request) {
	home, _ := os.UserHomeDir()
	path := home + "/dev/orchard/main/eng/http/http-client.private.env.json"
	data, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "token file not found", http.StatusNotFound)
		return
	}
	// The file has control chars that break standard JSON parsing.
	// Find the "local" section, then extract the ORCHARD_AUTH_TOKEN value.
	s := string(data)
	localIdx := strings.Index(s, `"local"`)
	if localIdx < 0 {
		http.Error(w, "no local section", http.StatusNotFound)
		return
	}
	localSection := s[localIdx:]
	tokenKey := `"ORCHARD_AUTH_TOKEN"`
	keyIdx := strings.Index(localSection, tokenKey)
	if keyIdx < 0 {
		http.Error(w, "token key not found in local", http.StatusNotFound)
		return
	}
	afterKey := localSection[keyIdx+len(tokenKey):]
	// Skip whitespace and colon
	qi := strings.Index(afterKey, `"`)
	if qi < 0 { http.Error(w, "no token value", http.StatusNotFound); return }
	tokenStart := afterKey[qi+1:]
	// JWT tokens only contain [A-Za-z0-9._-]. Scan until we hit anything else.
	var b strings.Builder
	for _, c := range tokenStart {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-' {
			b.WriteRune(c)
		}
		// Skip whitespace/control chars silently (line wraps)
		// Stop at quotes or other structural chars
		if c == '"' || c == ',' || c == '}' {
			break
		}
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(b.String()))
}
