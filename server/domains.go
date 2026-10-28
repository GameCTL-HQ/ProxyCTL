package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// DomainStore persists the operator's base domains (e.g. "examplelabs.cc").
// These power the DNS-name dropdown in the UI so a new entry's DNS name can
// be composed as <name>.<domain>. Subdomains cannot be enumerated over DNS;
// listing/auto-discovery of existing records is a later Cloudflare-API job.
// Persisted as a plain JSON string list next to the entries store.
type DomainStore struct {
	path string
	mu   sync.RWMutex
	list []string
}

func NewDomainStore(path string) (*DomainStore, error) {
	s := &DomainStore{path: path, list: []string{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &s.list); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return s, nil
}

// normalizeDomain strips a leading wildcard / dot and lowercases, so
// "*.examplelabs.cc" and "examplelabs.cc" both store as "examplelabs.cc".
func normalizeDomain(d string) string {
	d = strings.TrimSpace(strings.ToLower(d))
	d = strings.TrimPrefix(d, "*.")
	d = strings.Trim(d, ".")
	return d
}

func validDomain(d string) bool {
	if d == "" || len(d) > 253 || !strings.Contains(d, ".") {
		return false
	}
	for _, r := range d {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-') {
			return false
		}
	}
	return true
}

func (s *DomainStore) List() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]string(nil), s.list...)
	sort.Strings(out)
	return out
}

func (s *DomainStore) flushLocked() error {
	b, err := json.MarshalIndent(s.list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *DomainStore) Add(d string) error {
	d = normalizeDomain(d)
	if !validDomain(d) {
		return fmt.Errorf("invalid domain %q", d)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.list {
		if x == d {
			return nil // idempotent
		}
	}
	s.list = append(s.list, d)
	return s.flushLocked()
}

func (s *DomainStore) Delete(d string) error {
	d = normalizeDomain(d)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.list[:0]
	for _, x := range s.list {
		if x != d {
			out = append(out, x)
		}
	}
	s.list = out
	return s.flushLocked()
}
