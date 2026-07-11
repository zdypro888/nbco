// Package keylock provides reference-counted per-key locks.
package keylock

import (
	"context"
	"sync"
)

// Map serializes work sharing the same key and removes idle lock entries.
// The zero value is ready for use.
type Map[K comparable] struct {
	mu      sync.Mutex
	entries map[K]*entry
}

type entry struct {
	token chan struct{}
	refs  int
}

// Acquire locks key and returns an idempotent release function.
func (m *Map[K]) Acquire(key K) func() {
	release, _ := m.AcquireContext(context.Background(), key)
	return release
}

// AcquireContext locks key like Acquire, but a caller waiting behind another
// holder can stop at its request deadline instead of occupying the queue forever.
func (m *Map[K]) AcquireContext(ctx context.Context, key K) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.entries == nil {
		m.entries = make(map[K]*entry)
	}
	e := m.entries[key]
	if e == nil {
		e = &entry{token: make(chan struct{}, 1)}
		e.token <- struct{}{}
		m.entries[key] = e
	}
	e.refs++
	m.mu.Unlock()

	select {
	case <-ctx.Done():
		m.dropRef(key, e)
		return nil, ctx.Err()
	case <-e.token:
	}
	if err := ctx.Err(); err != nil {
		e.token <- struct{}{}
		m.dropRef(key, e)
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			e.token <- struct{}{}
			m.dropRef(key, e)
		})
	}, nil
}

func (m *Map[K]) dropRef(key K, e *entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.refs--
	if e.refs == 0 && m.entries[key] == e {
		delete(m.entries, key)
	}
}

// Len returns the number of active or waiting keys. It is intended for diagnostics.
func (m *Map[K]) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}
