//go:build windows

package daemon

import (
	"encoding/json"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/jack-work/angl/catalog"
)

// InventoryItem is the structured representation rendered by angl ls/listen.
// Metadata is included in the stream so a listener sees catalog changes through
// the same ordered, versioned channel as process lifecycle changes.
type InventoryItem struct {
	ProcessStatus
	Metadata map[string]string `json:"metadata,omitempty"`
}

// InventorySnapshot is a complete inventory at Version.
type InventorySnapshot struct {
	Version uint64          `json:"version"`
	Items   []InventoryItem `json:"items"`
}

// InventoryPatch transforms BaseVersion into Version. Upsert contains complete
// records for added or changed angls; Removed contains names to delete.
type InventoryPatch struct {
	BaseVersion uint64          `json:"base_version"`
	Version     uint64          `json:"version"`
	Upsert      []InventoryItem `json:"upsert,omitempty"`
	Removed     []string        `json:"removed,omitempty"`
}

// InventoryUpdate carries either a delta or a complete replacement. A complete
// snapshot is used when it is smaller than a patch or a slow listener misses a
// delta, making recovery possible without opening a second connection.
type InventoryUpdate struct {
	Type     string             `json:"type"`
	Snapshot *InventorySnapshot `json:"snapshot,omitempty"`
	Patch    *InventoryPatch    `json:"patch,omitempty"`
}

// ListenRegistration is the response to the long-lived listen RPC. Updates
// after this response arrive as list.update JSON-RPC notifications.
type ListenRegistration struct {
	ListenerID uint64            `json:"listener_id"`
	Snapshot   InventorySnapshot `json:"snapshot"`
}

type inventoryStream struct {
	mu        sync.Mutex
	version   uint64
	items     []InventoryItem
	nextID    uint64
	listeners map[uint64]chan InventoryUpdate
}

func (d *Daemon) initInventoryStream() error {
	items, err := d.readInventory()
	if err != nil {
		return err
	}
	d.inventory.mu.Lock()
	d.inventory.version = 1
	d.inventory.items = items
	if d.inventory.listeners == nil {
		d.inventory.listeners = make(map[uint64]chan InventoryUpdate)
	}
	d.inventory.mu.Unlock()
	return nil
}

// watchInventory turns mutable process state and catalog metadata into one
// ordered stream. Polling here is intentional because catalog.json is
// independently and atomically edited by short-lived CLI processes. Elapsed
// fields are derived client-side from stable timestamps.
func (d *Daemon) watchInventory(done <-chan struct{}) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if !d.hasInventoryListeners() {
				continue
			}
			if err := d.refreshInventory(); err != nil {
				d.logger.Printf("inventory refresh: %v", err)
			}
		}
	}
}

func (d *Daemon) hasInventoryListeners() bool {
	d.inventory.mu.Lock()
	defer d.inventory.mu.Unlock()
	return len(d.inventory.listeners) > 0
}

func (d *Daemon) readInventory() ([]InventoryItem, error) {
	statuses := d.List()
	store, err := catalog.Load(catalog.DefaultPath())
	if err != nil {
		// Metadata must never prevent supervision or listener registration.
		d.logger.Printf("warning: load inventory metadata: %v", err)
		store = catalog.New()
	}
	items := make([]InventoryItem, 0, len(statuses))
	for _, status := range statuses {
		status.Args = append([]string(nil), status.Args...)
		items = append(items, InventoryItem{
			ProcessStatus: status,
			Metadata:      cloneMetadata(store.Labels[status.Name]),
		})
	}
	return items, nil
}

func (d *Daemon) refreshInventory() error {
	next, err := d.readInventory()
	if err != nil {
		return err
	}

	s := &d.inventory
	s.mu.Lock()
	defer s.mu.Unlock()
	if inventoryEqual(s.items, next) {
		return nil
	}

	base := s.version
	s.version++
	patch := diffInventory(base, s.version, s.items, next)
	s.items = next
	update := compactInventoryUpdate(patch, InventorySnapshot{Version: s.version, Items: cloneInventory(next)})

	for _, ch := range s.listeners {
		select {
		case ch <- update:
		default:
			// A listener can miss a patch while its terminal is busy. Replace
			// the queued delta with a complete current snapshot so it can
			// recover without violating the base-version contract.
			select {
			case <-ch:
			default:
			}
			ch <- InventoryUpdate{
				Type:     "snapshot",
				Snapshot: &InventorySnapshot{Version: s.version, Items: cloneInventory(next)},
			}
		}
	}
	return nil
}

func (d *Daemon) SubscribeInventory() (uint64, InventorySnapshot, <-chan InventoryUpdate) {
	// Refresh before registering so a daemon that had no listeners still gives
	// the new client a current initial snapshot.
	if err := d.refreshInventory(); err != nil {
		d.logger.Printf("inventory subscribe refresh: %v", err)
	}
	s := &d.inventory
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listeners == nil {
		s.listeners = make(map[uint64]chan InventoryUpdate)
	}
	s.nextID++
	id := s.nextID
	ch := make(chan InventoryUpdate, 1)
	s.listeners[id] = ch
	return id, InventorySnapshot{Version: s.version, Items: cloneInventory(s.items)}, ch
}

func (d *Daemon) UnsubscribeInventory(id uint64) {
	d.inventory.mu.Lock()
	delete(d.inventory.listeners, id)
	d.inventory.mu.Unlock()
}

func inventoryEqual(oldItems, newItems []InventoryItem) bool {
	if len(oldItems) != len(newItems) {
		return false
	}
	for i := range oldItems {
		if !inventoryItemEqual(oldItems[i], newItems[i]) {
			return false
		}
	}
	return true
}

func inventoryItemEqual(oldItem, newItem InventoryItem) bool {
	// Elapsed strings are a projection of the stable timestamps. Clients update
	// them locally, avoiding an all-row RPC patch every second.
	oldItem.Uptime, newItem.Uptime = "", ""
	oldItem.Lifetime, newItem.Lifetime = "", ""
	oldItem.NextRunIn, newItem.NextRunIn = "", ""
	return reflect.DeepEqual(oldItem, newItem)
}

func diffInventory(base, version uint64, oldItems, newItems []InventoryItem) InventoryPatch {
	oldByName := make(map[string]InventoryItem, len(oldItems))
	newByName := make(map[string]InventoryItem, len(newItems))
	for _, item := range oldItems {
		oldByName[item.Name] = item
	}
	for _, item := range newItems {
		newByName[item.Name] = item
	}

	patch := InventoryPatch{BaseVersion: base, Version: version}
	for _, item := range newItems {
		if old, ok := oldByName[item.Name]; !ok || !inventoryItemEqual(old, item) {
			patch.Upsert = append(patch.Upsert, cloneInventoryItem(item))
		}
	}
	for name := range oldByName {
		if _, ok := newByName[name]; !ok {
			patch.Removed = append(patch.Removed, name)
		}
	}
	sort.Strings(patch.Removed)
	return patch
}

func compactInventoryUpdate(patch InventoryPatch, snapshot InventorySnapshot) InventoryUpdate {
	patchBytes, _ := json.Marshal(patch)
	snapshotBytes, _ := json.Marshal(snapshot)
	if len(snapshotBytes) <= len(patchBytes) {
		return InventoryUpdate{Type: "snapshot", Snapshot: &snapshot}
	}
	return InventoryUpdate{Type: "patch", Patch: &patch}
}

func cloneInventory(items []InventoryItem) []InventoryItem {
	cloned := make([]InventoryItem, len(items))
	for i, item := range items {
		cloned[i] = cloneInventoryItem(item)
	}
	return cloned
}

func cloneInventoryItem(item InventoryItem) InventoryItem {
	item.Args = append([]string(nil), item.Args...)
	item.Metadata = cloneMetadata(item.Metadata)
	if item.Endpoint != nil {
		endpoint := *item.Endpoint
		item.Endpoint = &endpoint
	}
	return item
}

func cloneMetadata(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(labels))
	for key, value := range labels {
		cloned[key] = value
	}
	return cloned
}
