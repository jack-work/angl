//go:build windows

package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestLoadMissingAndLegacyCatalog(t *testing.T) {
	missing, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if missing.Version != SchemaVersion || missing.Labels == nil || missing.Views == nil {
		t.Fatalf("missing catalog = %#v", missing)
	}

	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(`{"labels":{"api":{"tier":"backend"}}}`), 0644); err != nil {
		t.Fatal(err)
	}
	legacy, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Version != SchemaVersion || legacy.Labels["api"]["tier"] != "backend" {
		t.Fatalf("legacy catalog = %#v", legacy)
	}
}

func TestAnnotateMergesAndCopiesLabels(t *testing.T) {
	store := New()
	labels := map[string]string{"tier": "backend"}
	if err := store.Annotate("api", labels); err != nil {
		t.Fatal(err)
	}
	labels["tier"] = "mutated"
	if err := store.Annotate("api", map[string]string{"owner": "platform"}); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"tier": "backend", "owner": "platform"}
	if !reflect.DeepEqual(store.Labels["api"], want) {
		t.Fatalf("labels = %#v, want %#v", store.Labels["api"], want)
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
	}{
		{name: "empty key", labels: map[string]string{"": "value"}},
		{name: "space key", labels: map[string]string{"bad key": "value"}},
		{name: "comma value", labels: map[string]string{"key": "a,b"}},
		{name: "space value", labels: map[string]string{"key": " value"}},
		{name: "control value", labels: map[string]string{"key": "line\nbreak"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateLabels(tt.labels); err == nil {
				t.Fatal("ValidateLabels unexpectedly succeeded")
			}
		})
	}
	if err := ValidateLabels(map[string]string{"app.kubernetes.io/name": "api-v2", "empty": ""}); err != nil {
		t.Fatalf("valid labels rejected: %v", err)
	}
}

func TestSelectorMatching(t *testing.T) {
	labels := map[string]string{"env": "dev", "tier": "backend", "owner": "platform"}
	tests := []struct {
		selector string
		want     bool
	}{
		{selector: "", want: true},
		{selector: "env=dev,tier=backend", want: true},
		{selector: "env!=prod,owner", want: true},
		{selector: "missing!=value", want: true},
		{selector: "!region", want: true},
		{selector: "env=prod", want: false},
		{selector: "env!=dev", want: false},
		{selector: "!owner", want: false},
		{selector: "region", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.selector, func(t *testing.T) {
			selector, err := ParseSelector(tt.selector)
			if err != nil {
				t.Fatal(err)
			}
			if got := selector.Matches(labels); got != tt.want {
				t.Fatalf("Matches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseSelectorRejectsInvalidInput(t *testing.T) {
	for _, selector := range []string{"env=dev,", "=dev", "!", "bad key=value", "!env=value"} {
		if _, err := ParseSelector(selector); err == nil {
			t.Errorf("ParseSelector(%q) unexpectedly succeeded", selector)
		}
	}
}

func TestResolveViewIsSortedAndFresh(t *testing.T) {
	store := New()
	if err := store.SaveView("backends", "tier=backend"); err != nil {
		t.Fatal(err)
	}
	items := []SelectorItem{
		{Name: "zeta", State: "running", Enabled: true, Kind: "persistent", Labels: map[string]string{"env": "prod", "tier": "backend"}},
		{Name: "alpha", State: "running", Enabled: true, Kind: "persistent", Labels: map[string]string{"env": "dev", "tier": "backend"}},
		{Name: "beta", State: "running", Enabled: true, Kind: "persistent", Labels: map[string]string{"env": "dev", "tier": "frontend"}},
	}

	got, err := store.ResolveView("backends", items)
	if err != nil {
		t.Fatal(err)
	}
	if names := selectorItemNames(got); !reflect.DeepEqual(names, []string{"alpha", "zeta"}) {
		t.Fatalf("first resolution = %v", names)
	}

	items[2].Labels["tier"] = "backend"
	got, err = store.ResolveView("backends", items)
	if err != nil {
		t.Fatal(err)
	}
	if names := selectorItemNames(got); !reflect.DeepEqual(names, []string{"alpha", "beta", "zeta"}) {
		t.Fatalf("fresh resolution = %v", names)
	}
}

func TestResolveUsesLiveInventoryFieldsAndIncludesUnlabelled(t *testing.T) {
	items := []SelectorItem{
		{Name: "worker", State: "stopped", Enabled: false, Kind: "persistent"},
		{Name: "api", State: "running", Enabled: true, Kind: "heartbeat", Labels: map[string]string{"stack": "apps"}},
		{Name: "stale", State: "running", Enabled: true, Kind: "persistent", Labels: map[string]string{"state": "stopped"}},
	}
	selector, err := ParseSelector("state=running,enabled=true")
	if err != nil {
		t.Fatal(err)
	}
	got := Resolve(selector, items)
	if names := selectorItemNames(got); !reflect.DeepEqual(names, []string{"api", "stale"}) {
		t.Fatalf("resolved names = %v", names)
	}
	if got[0].Labels == nil || items[1].Labels == nil {
		t.Fatal("labels unexpectedly nil")
	}
	got[0].Labels["stack"] = "changed"
	if items[1].Labels["stack"] != "apps" {
		t.Fatal("Resolve aliased input labels")
	}
}

func TestSaveProducesDeterministicJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	store := New()
	store.Labels["zeta"] = map[string]string{"z": "last", "a": "first"}
	store.Labels["alpha"] = map[string]string{"tier": "backend"}
	store.Views["prod"] = "env=prod"

	if err := Save(path, store); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, store); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("repeated saves differ")
	}
	if strings.Index(string(first), `"alpha"`) > strings.Index(string(first), `"zeta"`) ||
		strings.Index(string(first), `"a"`) > strings.Index(string(first), `"z"`) {
		t.Fatalf("map keys not sorted:\n%s", first)
	}
	var decoded Store
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}

func TestUpdateSerializesConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	const writers = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			name := "worker-" + string(rune('a'+i))
			errs <- Update(path, func(store *Store) error {
				return store.Annotate(name, map[string]string{"group": "test"})
			})
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.Labels) != writers {
		t.Fatalf("got %d entries, want %d", len(store.Labels), writers)
	}
}

func selectorItemNames(items []SelectorItem) []string {
	names := make([]string, len(items))
	for i, item := range items {
		names[i] = item.Name
	}
	return names
}
