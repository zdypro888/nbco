// Package keylock provides reference-counted per-key locks.
package keylock

import "sync"

// Map serializes work sharing the same key and removes idle lock entries.
// The zero value is ready for use.
type Map[K comparable] struct {
	mu      sync.Mutex
	entries map[K]*entry
}

type entry struct {
	mu   sync.Mutex
	refs int
}

// Acquire locks key and returns an idempotent release function.
func (m *Map[K]) Acquire(key K) func() {
	m.mu.Lock()
	if m.entries == nil {
		m.entries = make(map[K]*entry)
	}
	e := m.entries[key]
	if e == nil {
		e = &entry{}
		m.entries[key] = e
	}
	e.refs++
	m.mu.Unlock()

	e.mu.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Unlock()
			m.mu.Lock()
			e.refs--
			if e.refs == 0 && m.entries[key] == e {
				delete(m.entries, key)
			}
			m.mu.Unlock()
		})
	}
}

// Len returns the number of active or waiting keys. It is intended for diagnostics.
func (m *Map[K]) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}
