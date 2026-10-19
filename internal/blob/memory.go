package blob

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"sync"
)

// Memory is an in-process Objects implementation for tests and dry runs. It
// copies on write and read so callers cannot mutate stored bytes.
type Memory struct {
	mu      sync.RWMutex
	objects map[string]memoryObject
}

func (m *Memory) PutImmutable(ctx context.Context, key string, data []byte, contentType string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.objects[key]; ok {
		if bytes.Equal(existing.data, data) {
			return nil
		}
		return errors.New("immutable object conflict")
	}
	m.objects[key] = memoryObject{data: append([]byte(nil), data...), contentType: contentType}
	return nil
}

type memoryObject struct {
	data        []byte
	contentType string
}

// NewMemory returns an empty in-memory object store.
func NewMemory() *Memory {
	return &Memory{objects: make(map[string]memoryObject)}
}

// Location identifies the backend in stored metadata.
func (m *Memory) Location() string { return "memory" }

// Put stores a copy of data under key, replacing any previous object.
func (m *Memory) Put(_ context.Context, key string, data []byte, contentType string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = memoryObject{data: append([]byte(nil), data...), contentType: contentType}
	return nil
}

// Read returns a copy of the object, or ErrNotFound.
func (m *Memory) Read(_ context.Context, key string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	obj, ok := m.objects[key]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), obj.data...), nil
}

// Delete removes an object; deleting an absent object is not an error.
func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

// Keys returns the stored keys in sorted order.
func (m *Memory) Keys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := make([]string, 0, len(m.objects))
	for key := range m.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// TotalBytes returns the summed size of every stored object.
func (m *Memory) TotalBytes() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var total int64
	for _, obj := range m.objects {
		total += int64(len(obj.data))
	}
	return total
}
