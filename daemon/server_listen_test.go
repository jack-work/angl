//go:build windows

package daemon

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"testing"
	"time"
)

func TestListenRPCRegistersAndStreamsNotification(t *testing.T) {
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	d.inventory.version = 1
	d.inventory.listeners = make(map[uint64]chan InventoryUpdate)

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	server := NewServer(d)
	done := make(chan struct{})
	go func() {
		server.handleConn(serverConn)
		close(done)
	}()

	if err := json.NewEncoder(clientConn).Encode(RPCRequest{JSONRPC: "2.0", Method: "listen", ID: 7}); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(clientConn)
	var response RPCResponse
	if err := decoder.Decode(&response); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(response.Result)
	if err != nil {
		t.Fatal(err)
	}
	var registration ListenRegistration
	if err := json.Unmarshal(raw, &registration); err != nil {
		t.Fatal(err)
	}
	if registration.ListenerID == 0 || registration.Snapshot.Version != 1 {
		t.Fatalf("registration = %#v", registration)
	}

	d.inventory.mu.Lock()
	updates := d.inventory.listeners[registration.ListenerID]
	d.inventory.mu.Unlock()
	updates <- InventoryUpdate{Type: "patch", Patch: &InventoryPatch{BaseVersion: 1, Version: 2}}

	var notification RPCRequest
	if err := decoder.Decode(&notification); err != nil {
		t.Fatal(err)
	}
	if notification.Method != "list.update" || notification.ID != nil {
		t.Fatalf("notification = %#v", notification)
	}
	var update InventoryUpdate
	if err := json.Unmarshal(notification.Params, &update); err != nil {
		t.Fatal(err)
	}
	if update.Patch == nil || update.Patch.BaseVersion != 1 || update.Patch.Version != 2 {
		t.Fatalf("update = %#v", update)
	}

	clientConn.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("listener RPC did not exit after disconnect")
	}
}
