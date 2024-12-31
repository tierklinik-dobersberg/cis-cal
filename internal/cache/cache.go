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

type Indexer[T any] interface {
	Update(values []T)
}

type Cache[T any] struct {
	name string
	log  *slog.Logger

	l         sync.RWMutex
	interval  time.Duration
	startOnce sync.Once
	wg        sync.WaitGroup
	trigger   chan struct{}

	values    []T
	lastFetch time.Time
	loader    Loader[T]

	indexLock sync.Mutex
	indexes   []Indexer[T]
}

func CreateIndex[K comparable, T any](cache *Cache[T], indexer func(T) (K, bool)) *Index[K, T] {
	i := NewIndex(indexer)

	cache.AddIndex(i)

	return i
}

func (cache *Cache[T]) AddIndex(index Indexer[T]) {
	cache.indexLock.Lock()
	cache.indexes = append(cache.indexes, index)
	cache.indexLock.Unlock()

	// immediately update the index
	values, _ := cache.Get()
	index.Update(values)
}

func (cache *Cache[T]) updateIndexes(values []T) {
	cache.indexLock.Lock()
	defer cache.indexLock.Unlock()

	for _, i := range cache.indexes {
		i.Update(values)
	}
}

func NewCache[T any](name string, interval time.Duration, loader Loader[T]) *Cache[T] {
	return &Cache[T]{
		name:     name,
		interval: interval,
		loader:   loader,
		trigger:  make(chan struct{}),
		log:      slog.With("name", name),
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
					c.log.Error("failed to update cache values", "error", err)
				} else {
					now := time.Now()

					c.l.Lock()
					c.values = values
					c.lastFetch = now
					c.l.Unlock()

					c.updateIndexes(values)

					c.log.Error("successfully updated cache values", "count", len(values))
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
