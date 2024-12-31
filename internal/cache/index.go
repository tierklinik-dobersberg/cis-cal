package cache

import "sync"

type Index[K comparable, T any] struct {
	l      sync.RWMutex
	values map[K]T

	indexer func(t T) (K, bool)
}

func NewIndex[K comparable, T any](indexer func(T) (K, bool)) *Index[K, T] {
	return &Index[K, T]{
		values:  make(map[K]T),
		indexer: indexer,
	}
}

func (i *Index[K, T]) Get(key K) (T, bool) {
	i.l.RLock()
	defer i.l.RUnlock()

	val, ok := i.values[key]

	return val, ok
}

func (i *Index[K, T]) Update(values []T) {
	m := make(map[K]T)
	for _, v := range values {
		k, ok := i.indexer(v)
		if !ok {
			continue
		}

		m[k] = v
	}

	i.l.Lock()
	defer i.l.Unlock()
	i.values = m
}
