//go:build windows

// Package catalog provides process-independent metadata and saved selectors for
// angls. Its JSON store is deliberately separate from daemon config and
// transient state, so annotation and query never reconcile or restart a process
// and can be used with an already-running older daemon.
package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
)

const SchemaVersion = 1

var processLock sync.Mutex

// Store is the on-disk metadata catalog. Labels are keyed first by angl name.
// Views contain saved selector strings and are evaluated when Materialize is
// called, so they cannot become stale.
type Store struct {
	Version int                          `json:"version"`
	Labels  map[string]map[string]string `json:"labels,omitempty"`
	Views   map[string]string            `json:"views,omitempty"`
}

// Match identifies a catalog entry selected by Query or Materialize.
type Match struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
}

var (
	validName     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	validLabelKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
)

func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "angl", "catalog.json")
}

func New() Store {
	return Store{
		Version: SchemaVersion,
		Labels:  make(map[string]map[string]string),
		Views:   make(map[string]string),
	}
}

// Load accepts a missing file as an empty catalog and rejects unknown future
// schema versions. Omitting version is accepted as version 1 for compatibility
// with early hand-authored catalogs.
func Load(path string) (Store, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return New(), nil
		}
		return Store{}, fmt.Errorf("read catalog: %w", err)
	}

	store := New()
	if err := json.Unmarshal(data, &store); err != nil {
		return Store{}, fmt.Errorf("parse catalog: %w", err)
	}
	if store.Version == 0 {
		store.Version = SchemaVersion
	}
	if store.Version != SchemaVersion {
		return Store{}, fmt.Errorf("unsupported catalog version %d", store.Version)
	}
	if store.Labels == nil {
		store.Labels = make(map[string]map[string]string)
	}
	if store.Views == nil {
		store.Views = make(map[string]string)
	}
	if err := store.Validate(); err != nil {
		return Store{}, err
	}
	return store, nil
}

func (s Store) Validate() error {
	if s.Version != SchemaVersion {
		return fmt.Errorf("unsupported catalog version %d", s.Version)
	}
	for name, labels := range s.Labels {
		if err := ValidateName(name); err != nil {
			return err
		}
		if err := ValidateLabels(labels); err != nil {
			return fmt.Errorf("angl %q: %w", name, err)
		}
	}
	for name, selector := range s.Views {
		if err := ValidateName(name); err != nil {
			return fmt.Errorf("view: %w", err)
		}
		if _, err := ParseSelector(selector); err != nil {
			return fmt.Errorf("view %q: %w", name, err)
		}
	}
	return nil
}

func ValidateName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid name %q (use letters, digits, '.', '_' or '-')", name)
	}
	return nil
}

func ValidateLabels(labels map[string]string) error {
	for key, value := range labels {
		if !validLabelKey.MatchString(key) {
			return fmt.Errorf("invalid label key %q (use letters, digits, '.', '_', '/' or '-')", key)
		}
		if strings.TrimSpace(value) != value {
			return fmt.Errorf("label %q value may not have leading or trailing whitespace", key)
		}
		if strings.Contains(value, ",") {
			return fmt.Errorf("label %q value may not contain ','", key)
		}
		for _, r := range value {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("label %q value may not contain control characters", key)
			}
		}
	}
	return nil
}

// Annotate merges labels into an angl's catalog entry. A nil map is rejected;
// callers should use RemoveLabels or Delete for explicit removal.
func (s *Store) Annotate(name string, labels map[string]string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if labels == nil {
		return fmt.Errorf("labels are required")
	}
	if err := ValidateLabels(labels); err != nil {
		return err
	}
	s.ensureMaps()
	merged := cloneLabels(s.Labels[name])
	for key, value := range labels {
		merged[key] = value
	}
	s.Labels[name] = merged
	return nil
}

func (s *Store) RemoveLabels(name string, keys ...string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	for _, key := range keys {
		if !validLabelKey.MatchString(key) {
			return fmt.Errorf("invalid label key %q", key)
		}
	}
	labels, ok := s.Labels[name]
	if !ok {
		return nil
	}
	for _, key := range keys {
		delete(labels, key)
	}
	if len(labels) == 0 {
		delete(s.Labels, name)
	}
	return nil
}

func (s *Store) Delete(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	delete(s.Labels, name)
	return nil
}

func (s *Store) SaveView(name, selector string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	parsed, err := ParseSelector(selector)
	if err != nil {
		return err
	}
	s.ensureMaps()
	s.Views[name] = parsed.String()
	return nil
}

func (s *Store) DeleteView(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	delete(s.Views, name)
	return nil
}

func (s Store) Query(selector string) ([]Match, error) {
	parsed, err := ParseSelector(selector)
	if err != nil {
		return nil, err
	}
	return s.query(parsed), nil
}

func (s Store) Materialize(view string) ([]Match, error) {
	selector, ok := s.Views[view]
	if !ok {
		return nil, fmt.Errorf("unknown view %q", view)
	}
	return s.Query(selector)
}

func (s Store) query(selector Selector) []Match {
	names := make([]string, 0, len(s.Labels))
	for name := range s.Labels {
		names = append(names, name)
	}
	sort.Strings(names)

	matches := make([]Match, 0)
	for _, name := range names {
		labels := s.Labels[name]
		if selector.Matches(labels) {
			matches = append(matches, Match{Name: name, Labels: cloneLabels(labels)})
		}
	}
	return matches
}

func (s *Store) ensureMaps() {
	if s.Version == 0 {
		s.Version = SchemaVersion
	}
	if s.Labels == nil {
		s.Labels = make(map[string]map[string]string)
	}
	if s.Views == nil {
		s.Views = make(map[string]string)
	}
}

func cloneLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return make(map[string]string)
	}
	copy := make(map[string]string, len(labels))
	for key, value := range labels {
		copy[key] = value
	}
	return copy
}

// Save writes deterministic indented JSON via an atomic replace. encoding/json
// sorts string-keyed maps, making catalog diffs stable.
func Save(path string, store Store) error {
	store.ensureMaps()
	if err := store.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

// Update serializes read-modify-write operations with a sidecar lock and then
// atomically replaces the catalog. The daemon never observes or owns this file.
func Update(path string, update func(*Store) error) error {
	if update == nil {
		return fmt.Errorf("update function is required")
	}
	processLock.Lock()
	defer processLock.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	lock, err := lockCatalog(path + ".lock")
	if err != nil {
		return err
	}
	defer unlockCatalog(lock)

	store, err := Load(path)
	if err != nil {
		return err
	}
	if err := update(&store); err != nil {
		return err
	}
	return Save(path, store)
}

func lockCatalog(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open catalog lock: %w", err)
	}
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped); err != nil {
		file.Close()
		return nil, fmt.Errorf("lock catalog: %w", err)
	}
	return file, nil
}

func unlockCatalog(file *os.File) {
	var overlapped windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
	_ = file.Close()
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(tmpPath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
