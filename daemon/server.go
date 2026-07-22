//go:build windows

package daemon

import (
	"context"
	"encoding/json"
	"net"
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

// Run serves the local control API over a Windows named pipe. The pipe is the
// only control plane: angl does not open a TCP port or expose an HTTP API.
func (s *Server) Run(ctx context.Context) error {
	listener, err := winio.ListenPipe(pipeName, &winio.PipeConfig{
		InputBufferSize:  4096,
		OutputBufferSize: 65536,
	})
	if err != nil {
		return err
	}
	defer listener.Close()

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	s.daemon.logger.Printf("listening on %s", pipeName)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.daemon.logger.Printf("pipe accept: %v", err)
			continue
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Minute))

	var req RPCRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		json.NewEncoder(conn).Encode(rpcErr(nil, -32700, "parse error"))
		return
	}
	if req.Method == "listen" {
		s.handleListen(conn, req)
		return
	}
	json.NewEncoder(conn).Encode(s.daemon.HandleRPC(req))
}

func (s *Server) handleListen(conn net.Conn, req RPCRequest) {
	// A listener is a separate long-lived client process. Clear the ordinary
	// RPC deadline; disconnects are discovered by the next notification write.
	conn.SetDeadline(time.Time{})
	id, snapshot, updates := s.daemon.SubscribeInventory()
	defer s.daemon.UnsubscribeInventory(id)

	encoder := json.NewEncoder(conn)
	registered := ListenRegistration{ListenerID: id, Snapshot: snapshot}
	if err := encoder.Encode(rpcOK(req.ID, registered)); err != nil {
		return
	}
	disconnected := make(chan struct{})
	go func() {
		var oneByte [1]byte
		_, _ = conn.Read(oneByte[:])
		close(disconnected)
	}()
	for {
		select {
		case update := <-updates:
			if err := encoder.Encode(rpcNotification("list.update", update)); err != nil {
				return
			}
		case <-disconnected:
			return
		}
	}
}

// IsDaemonRunning checks whether another daemon owns the control pipe.
func IsDaemonRunning() bool {
	timeout := 2 * time.Second
	conn, err := winio.DialPipe(pipeName, &timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
