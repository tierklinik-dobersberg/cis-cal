package cache

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

type Loader[T any] interface {
	Load(context.Context) ([]T, error)
}

type LoaderFunc[T any] func(context.Context) ([]T, error)

func (lf LoaderFunc[T]) Load(ctx context.Context) ([]T, error) {
	return lf(ctx)
}

type Cache[T any] struct {
	l         sync.RWMutex
	interval  time.Duration
	startOnce sync.Once
	wg        sync.WaitGroup
	trigger   chan struct{}

	values    []T
	lastFetch time.Time
	loader    Loader[T]

	indexLock sync.Mutex
	indexes   []*Index[T]
}

type Index[T any] struct {
	l      sync.RWMutex
	values map[string]T

	indexer func(t T) (string, bool)
}

func NewIndex[T any](indexer func(T) (string, bool)) *Index[T] {
	return &Index[T]{
		values: make(map[string]T),
	}
}

func (i *Index[T]) update(values []T) {
	m := make(map[string]T)
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

func (cache *Cache[T]) AddIndex(index *Index[T]) {
	cache.indexLock.Lock()
	cache.indexes = append(cache.indexes, index)
	cache.indexLock.Unlock()

	// immediately update the index
	values, _ := cache.Get()
	go index.update(values)
}

func (cache *Cache[T]) updateIndexes(values []T) {
	cache.indexLock.Lock()
	defer cache.indexLock.Unlock()

	for _, i := range cache.indexes {
		i.update(values)
	}
}

func NewCache[T any](interval time.Duration, loader Loader[T]) *Cache[T] {
	return &Cache[T]{
		interval: interval,
		loader:   loader,
		trigger:  make(chan struct{}),
	}
}

func (c *Cache[T]) Get() ([]T, bool) {
	c.l.RLock()
	defer c.l.RUnlock()

	isStale := time.Since(c.lastFetch) > c.interval

	res := make([]T, len(c.values))

	copy(res, c.values)

	return res, isStale
}

func (c *Cache[T]) TriggerSync() {
	c.trigger <- struct{}{}
}

func (c *Cache[T]) Wait() {
	c.wg.Wait()
}

func (c *Cache[T]) Start(ctx context.Context) {
	c.startOnce.Do(func() {
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()

			ticker := time.NewTicker(c.interval)

			for {
				fetchCtx, cancel := context.WithTimeout(ctx, c.interval)

				values, err := c.loader.Load(fetchCtx)
				cancel()

				if err != nil {
					slog.Error("failed to update cache values", "error", err)
				} else {
					now := time.Now()

					c.l.Lock()
					c.values = values
					c.lastFetch = now
					c.l.Unlock()

					c.updateIndexes(values)

					slog.Error("successfully updated cache values", "count", len(values))
				}

				select {
				case <-ticker.C:
				case <-c.trigger:
				case <-ctx.Done():
					return
				}

			}
		}()
	})
}
