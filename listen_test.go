//go:build windows

package main

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jack-work/angl/daemon"
)

func TestListenModelAppliesVersionedPatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := newListenModel(ctx, cancel)
	model.applySnapshot(daemon.InventorySnapshot{Version: 4, Items: []daemon.InventoryItem{
		{ProcessStatus: daemon.ProcessStatus{Name: "old", State: daemon.StateStopped}},
	}}, false)

	err := model.applyUpdate(daemon.InventoryUpdate{Type: "patch", Patch: &daemon.InventoryPatch{
		BaseVersion: 4,
		Version:     5,
		Removed:     []string{"old"},
		Upsert: []daemon.InventoryItem{
			{ProcessStatus: daemon.ProcessStatus{Name: "new", State: daemon.StateRunning, PID: 42}, Metadata: map[string]string{"stack": "apps"}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if model.version != 5 || len(model.items) != 1 || model.items["new"].PID != 42 {
		t.Fatalf("model = version %d items %#v", model.version, model.items)
	}
	if _, changed := model.changed["new"]; !changed {
		t.Fatal("new row was not marked changed")
	}
	if got := strings.Join(model.recent, ","); got != "+ new,- old" && got != "- old,+ new" {
		t.Fatalf("recent changes = %q", got)
	}
}

func TestListenModelRejectsPatchGap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := newListenModel(ctx, cancel)
	model.version = 2
	if err := model.applyUpdate(daemon.InventoryUpdate{Type: "patch", Patch: &daemon.InventoryPatch{BaseVersion: 1, Version: 3}}); err == nil {
		t.Fatal("version gap unexpectedly accepted")
	}
}

func TestListenEnterExpandsFullVisibleDetails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := newListenModel(ctx, cancel)
	model.width = 180
	model.applySnapshot(daemon.InventorySnapshot{Version: 1, Items: []daemon.InventoryItem{{
		ProcessStatus: daemon.ProcessStatus{
			Name: "api", State: daemon.StateRunning, Command: "api.exe",
			Args: []string{"a full argument"}, Charge: "a full charge",
		},
		Metadata: map[string]string{"stack": "apps"},
	}}}, false)

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	expanded := updated.(listenModel)
	if !expanded.expanded {
		t.Fatal("enter did not expand selected details")
	}
	view := expanded.View()
	for _, want := range []string{"a full argument", "a full charge", "stack=apps"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expanded view missing %q:\n%s", want, view)
		}
	}
}

func TestListenActionsUseRealCLIVerbs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := newListenModel(ctx, cancel)
	model.items["api"] = daemon.InventoryItem{ProcessStatus: daemon.ProcessStatus{Name: "api", State: daemon.StateBackoff}}

	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	got := updated.(listenModel)
	if got.action != "exec" || cmd == nil {
		t.Fatalf("exec action = %q, cmd nil = %v", got.action, cmd == nil)
	}

	model.action = ""
	updated, cmd = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	got = updated.(listenModel)
	if got.action != "stop" || cmd == nil {
		t.Fatalf("stop action = %q, cmd nil = %v", got.action, cmd == nil)
	}
}

func TestListenSpaceVisualAndEscapeSelection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := newListenModel(ctx, cancel)
	for _, name := range []string{"a", "b", "c"} {
		model.items[name] = daemon.InventoryItem{ProcessStatus: daemon.ProcessStatus{Name: name}}
	}

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeySpace})
	model = updated.(listenModel)
	if !model.marked["a"] {
		t.Fatal("space did not select current row")
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	model = updated.(listenModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	model = updated.(listenModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	model = updated.(listenModel)
	for _, name := range []string{"a", "b", "c"} {
		if !model.marked[name] {
			t.Fatalf("visual selection missing %s: %#v", name, model.marked)
		}
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(listenModel)
	if len(model.marked) != 0 || model.visual {
		t.Fatalf("escape did not clear selection: %#v", model.marked)
	}
}

func TestListenGGAndGNavigate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := newListenModel(ctx, cancel)
	for _, name := range []string{"a", "b", "c"} {
		model.items[name] = daemon.InventoryItem{ProcessStatus: daemon.ProcessStatus{Name: name}}
	}
	model.selected = 1
	for _, key := range []rune{'g', 'g'} {
		updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
		model = updated.(listenModel)
	}
	if model.selected != 0 {
		t.Fatalf("gg selected %d, want 0", model.selected)
	}
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	if got := updated.(listenModel).selected; got != 2 {
		t.Fatalf("G selected %d, want 2", got)
	}
}

func TestListenDeleteConfirmationDefaultsToNo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := newListenModel(ctx, cancel)
	model.items["api"] = daemon.InventoryItem{ProcessStatus: daemon.ProcessStatus{Name: "api"}}

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	model = updated.(listenModel)
	if model.confirm == nil || model.confirm.yes {
		t.Fatal("delete confirmation missing or did not default to No")
	}
	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(listenModel)
	if model.confirm != nil || cmd != nil || model.action != "" {
		t.Fatal("Enter on default No did not cancel")
	}

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	model = updated.(listenModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	model = updated.(listenModel)
	updated, cmd = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(listenModel)
	if model.action != "delete" || cmd == nil {
		t.Fatalf("confirmed delete action = %q, cmd nil = %v", model.action, cmd == nil)
	}
}

func TestListenViewRendersInventoryAndMetadata(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := newListenModel(ctx, cancel)
	model.width = 180
	model.connected = true
	model.applySnapshot(daemon.InventorySnapshot{Version: 9, Items: []daemon.InventoryItem{
		{ProcessStatus: daemon.ProcessStatus{Name: "api", State: daemon.StateRunning, PID: 42, Command: "api.exe"}, Metadata: map[string]string{"stack": "apps"}},
	}}, false)

	view := model.View()
	for _, want := range []string{"angl listen", "v9", "api", "running", "stack=apps"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
	if updated.(listenModel).selected != 0 {
		t.Fatal("single-row selection moved out of range")
	}
}
