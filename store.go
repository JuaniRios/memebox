package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind tells the converter which export formats make sense for an item.
type Kind string

const (
	KindStill    Kind = "still"
	KindAnimated Kind = "animated"
	KindVideo    Kind = "video"
)

type Item struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Tags     []string  `json:"tags"`
	Kind     Kind      `json:"kind"`
	Ext      string    `json:"ext"`
	Width    int       `json:"width"`
	Height   int       `json:"height"`
	Duration float64   `json:"duration,omitempty"`
	HasAudio bool      `json:"hasAudio,omitempty"`
	Size     int64     `json:"size"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"lastUsed,omitzero"`
	Uses     int       `json:"uses"`
}

var ErrNotFound = errors.New("not found")

// Store keeps the library index in one JSON file next to the media
// directories. A personal meme library is small enough that rewriting the
// whole index on every change is simpler than running a database.
type Store struct {
	dir   string
	mu    sync.RWMutex
	items map[string]*Item
}

func OpenStore(dir string) (*Store, error) {
	for _, sub := range []string{"originals", "previews", "exports", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	s := &Store{dir: dir, items: map[string]*Item{}}
	data, err := os.ReadFile(s.indexPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		var list []*Item
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, fmt.Errorf("read %s: %w", s.indexPath(), err)
		}
		for _, it := range list {
			s.items[it.ID] = it
		}
	}
	return s, nil
}

func (s *Store) indexPath() string { return filepath.Join(s.dir, "index.json") }

func (s *Store) OriginalPath(it *Item) string {
	return filepath.Join(s.dir, "originals", it.ID+"."+it.Ext)
}

func (s *Store) PreviewPath(id string) string {
	return filepath.Join(s.dir, "previews", id+".webp")
}

func (s *Store) ExportDir(id string) string { return filepath.Join(s.dir, "exports", id) }

func (s *Store) TmpDir() string { return filepath.Join(s.dir, "tmp") }

// saveLocked writes the index atomically. Callers hold s.mu.
func (s *Store) saveLocked() error {
	list := make([]*Item, 0, len(s.items))
	for _, it := range s.items {
		list = append(list, it)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Created.Before(list[j].Created) })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.indexPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.indexPath())
}

func (s *Store) List() []Item {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Item, 0, len(s.items))
	for _, it := range s.items {
		out = append(out, *it)
	}
	return out
}

func (s *Store) Get(id string) (Item, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	it, ok := s.items[id]
	if !ok {
		return Item{}, ErrNotFound
	}
	return *it, nil
}

func (s *Store) Add(it Item) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[it.ID] = &it
	return s.saveLocked()
}

func (s *Store) Update(id string, fn func(*Item)) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	if !ok {
		return Item{}, ErrNotFound
	}
	fn(it)
	return *it, s.saveLocked()
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	it, ok := s.items[id]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	delete(s.items, id)
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	os.Remove(s.OriginalPath(it))
	os.Remove(s.PreviewPath(id))
	os.RemoveAll(s.ExportDir(id))
	return nil
}

func newID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// normalizeTags lowercases, trims, and de-duplicates tags so search and
// the tag filter treat "Cat" and "cat " as the same tag.
func normalizeTags(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range in {
		t = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(t), "#")))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}
