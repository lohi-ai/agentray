package engine

import (
	"encoding/json"
	"slices"
	"sync"

	"github.com/lohi-ai/agentray/ai"
)

// List preserves collection identity when entries or length change. Container
// operations are synchronized; callers still synchronize edits to shared items.
// Values returns a detached, dense slice; Has distinguishes holes from zero T.
type List[T any] struct {
	mu     sync.RWMutex
	length int
	values map[int]T
}

type MessageList = List[*ai.Message]
type ToolList = List[*Tool]

func NewList[T any](values ...T) *List[T] {
	list := &List[T]{length: len(values), values: make(map[int]T, len(values))}
	for i, value := range values {
		list.values[i] = value
	}
	return list
}

func (l *List[T]) Len() int {
	if l == nil {
		return 0
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.length
}

func (l *List[T]) Get(index int) T {
	if l == nil {
		var zero T
		return zero
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.values[index]
}

func (l *List[T]) Has(index int) bool {
	if l == nil {
		return false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	_, ok := l.values[index]
	return ok
}

func (l *List[T]) Set(index int, value T) {
	if index < 0 || uint64(index) >= 4294967295 {
		panic("Invalid array index")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.values == nil {
		l.values = make(map[int]T)
	}
	l.values[index] = value
	if index >= l.length {
		l.length = index + 1
	}
}

func (l *List[T]) Delete(index int) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.values, index)
}

func (l *List[T]) SetLength(length int) {
	if length < 0 || uint64(length) > 4294967295 {
		panic("Invalid array length")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for index := range l.values {
		if index >= length {
			delete(l.values, index)
		}
	}
	l.length = length
}

func (l *List[T]) Append(values ...T) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if uint64(l.length)+uint64(len(values)) > 4294967295 {
		panic("Invalid array length")
	}
	if l.values == nil {
		l.values = make(map[int]T)
	}
	for _, value := range values {
		l.values[l.length] = value
		l.length++
	}
	return l.length
}

func (l *List[T]) Keys() []int {
	keys := []int{}
	if l == nil {
		return keys
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	for index := range l.values {
		keys = append(keys, index)
	}
	slices.Sort(keys)
	return keys
}

func (l *List[T]) Values() []T {
	if l == nil {
		return []T{}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	values := make([]T, l.length)
	for index, value := range l.values {
		values[index] = value
	}
	return values
}

func (l *List[T]) MarshalJSON() ([]byte, error) {
	// Release the list lock before serializing items; their graph is user-owned.
	values := []any{}
	if l != nil {
		l.mu.RLock()
		values = make([]any, l.length)
		for index, value := range l.values {
			values[index] = value
		}
		l.mu.RUnlock()
	}
	return json.Marshal(values)
}

func (l *List[T]) UnmarshalJSON(raw []byte) error {
	var values []T
	if err := json.Unmarshal(raw, &values); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.length, l.values = len(values), make(map[int]T, len(values))
	for index, value := range values {
		l.values[index] = value
	}
	return nil
}

// Clone copies the outer list, retaining item references and sparse slots.
func (l *List[T]) Clone() *List[T] {
	copied := NewList[T]()
	if l == nil {
		return copied
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	copied.length = l.length
	for index, value := range l.values {
		copied.values[index] = value
	}
	return copied
}
