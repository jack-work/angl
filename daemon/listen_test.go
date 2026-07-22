//go:build windows

package daemon

import "testing"

func TestDiffInventoryAppliesAsVersionedUpsertsAndRemoves(t *testing.T) {
	oldItems := []InventoryItem{
		{ProcessStatus: ProcessStatus{Name: "gone", State: StateStopped}},
		{ProcessStatus: ProcessStatus{Name: "same", State: StateRunning, PID: 1}},
		{ProcessStatus: ProcessStatus{Name: "updated", State: StateRunning, PID: 2}, Metadata: map[string]string{"role": "old"}},
	}
	newItems := []InventoryItem{
		{ProcessStatus: ProcessStatus{Name: "added", State: StateStarting}},
		{ProcessStatus: ProcessStatus{Name: "same", State: StateRunning, PID: 1}},
		{ProcessStatus: ProcessStatus{Name: "updated", State: StateBackoff}, Metadata: map[string]string{"role": "new"}},
	}

	patch := diffInventory(7, 8, oldItems, newItems)
	if patch.BaseVersion != 7 || patch.Version != 8 {
		t.Fatalf("versions = %d -> %d", patch.BaseVersion, patch.Version)
	}
	if len(patch.Upsert) != 2 || patch.Upsert[0].Name != "added" || patch.Upsert[1].Name != "updated" {
		t.Fatalf("upsert = %#v", patch.Upsert)
	}
	if len(patch.Removed) != 1 || patch.Removed[0] != "gone" {
		t.Fatalf("removed = %#v", patch.Removed)
	}
}

func TestInventoryEqualIgnoresClockFields(t *testing.T) {
	oldItems := []InventoryItem{{ProcessStatus: ProcessStatus{Name: "api", State: StateRunning, Uptime: "1s", Lifetime: "2s"}}}
	newItems := []InventoryItem{{ProcessStatus: ProcessStatus{Name: "api", State: StateRunning, Uptime: "2s", Lifetime: "3s"}}}
	if !inventoryEqual(oldItems, newItems) {
		t.Fatal("clock-only change was treated as a lifecycle change")
	}
}

func TestCompactInventoryUpdateChoosesSnapshotWhenPatchIsLarger(t *testing.T) {
	item := InventoryItem{ProcessStatus: ProcessStatus{Name: "api", State: StateRunning}}
	patch := InventoryPatch{BaseVersion: 1, Version: 2, Upsert: []InventoryItem{item}, Removed: []string{"an-unnecessarily-long-removed-name"}}
	snapshot := InventorySnapshot{Version: 2, Items: []InventoryItem{item}}
	update := compactInventoryUpdate(patch, snapshot)
	if update.Type != "snapshot" || update.Snapshot == nil {
		t.Fatalf("update = %#v", update)
	}
}
