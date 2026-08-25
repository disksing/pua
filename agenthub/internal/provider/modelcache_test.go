package provider

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disksing/pua/agenthub/internal/config"
)

func cacheTestProvider() config.Provider {
	return config.Provider{ID: "kimi", Type: "kimi"}
}

func TestModelCacheCachesSuccess(t *testing.T) {
	var calls atomic.Int32
	cache := newModelCache(time.Hour, time.Hour, func(context.Context, config.Provider) ([]Model, error) {
		calls.Add(1)
		return []Model{{ID: "m1", Label: "M1"}}, nil
	})
	for i := 0; i < 3; i++ {
		models, err := cache.Models(context.Background(), cacheTestProvider())
		if err != nil || len(models) != 1 || models[0].ID != "m1" {
			t.Fatalf("models = %v, err = %v", models, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("underlying calls = %d, want 1", calls.Load())
	}
}

func TestModelCacheEmptyListIsCached(t *testing.T) {
	var calls atomic.Int32
	cache := newModelCache(time.Hour, time.Hour, func(context.Context, config.Provider) ([]Model, error) {
		calls.Add(1)
		return []Model{}, nil
	})
	for i := 0; i < 2; i++ {
		models, err := cache.Models(context.Background(), cacheTestProvider())
		if err != nil || len(models) != 0 {
			t.Fatalf("models = %v, err = %v", models, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("underlying calls = %d, want 1", calls.Load())
	}
}

func TestModelCacheErrorExpiresFast(t *testing.T) {
	var calls atomic.Int32
	cache := newModelCache(time.Hour, 40*time.Millisecond, func(context.Context, config.Provider) ([]Model, error) {
		calls.Add(1)
		return nil, modelError(ModelErrUpstream, "broken")
	})
	if _, err := cache.Models(context.Background(), cacheTestProvider()); err == nil {
		t.Fatal("want error")
	}
	// The error itself is cached briefly.
	if _, err := cache.Models(context.Background(), cacheTestProvider()); err == nil {
		t.Fatal("want cached error")
	}
	if calls.Load() != 1 {
		t.Fatalf("underlying calls = %d, want 1 (error cached)", calls.Load())
	}
	time.Sleep(60 * time.Millisecond)
	if _, err := cache.Models(context.Background(), cacheTestProvider()); err == nil {
		t.Fatal("want error after retry")
	}
	if calls.Load() != 2 {
		t.Fatalf("underlying calls = %d, want 2 (error expired)", calls.Load())
	}
}

func TestModelCacheInvalidateAll(t *testing.T) {
	var calls atomic.Int32
	cache := newModelCache(time.Hour, time.Hour, func(context.Context, config.Provider) ([]Model, error) {
		calls.Add(1)
		return []Model{{ID: "m1"}}, nil
	})
	if _, err := cache.Models(context.Background(), cacheTestProvider()); err != nil {
		t.Fatal(err)
	}
	cache.InvalidateAll()
	if _, err := cache.Models(context.Background(), cacheTestProvider()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("underlying calls = %d, want 2 after invalidation", calls.Load())
	}
}

func TestModelCacheCommandChangeMisses(t *testing.T) {
	var calls atomic.Int32
	cache := newModelCache(time.Hour, time.Hour, func(context.Context, config.Provider) ([]Model, error) {
		calls.Add(1)
		return []Model{{ID: "m1"}}, nil
	})
	if _, err := cache.Models(context.Background(), cacheTestProvider()); err != nil {
		t.Fatal(err)
	}
	changed := cacheTestProvider()
	changed.Command = "/custom/kimi"
	if _, err := cache.Models(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("underlying calls = %d, want 2 (command changed)", calls.Load())
	}
}

func TestModelCacheSingleflight(t *testing.T) {
	var calls atomic.Int32
	gate := make(chan struct{})
	cache := newModelCache(time.Hour, time.Hour, func(context.Context, config.Provider) ([]Model, error) {
		calls.Add(1)
		<-gate
		return []Model{{ID: "m1"}}, nil
	})
	const workers = 8
	var group sync.WaitGroup
	results := make([][]Model, workers)
	failures := make([]error, workers)
	for i := 0; i < workers; i++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			results[index], failures[index] = cache.Models(context.Background(), cacheTestProvider())
		}(i)
	}
	// Let all workers pile onto the same inflight call, then release it.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	group.Wait()
	if calls.Load() != 1 {
		t.Fatalf("underlying calls = %d, want 1 (singleflight)", calls.Load())
	}
	for i := 0; i < workers; i++ {
		if failures[i] != nil || len(results[i]) != 1 {
			t.Fatalf("worker %d: models = %v, err = %v", i, results[i], failures[i])
		}
	}
}

func TestModelCacheReturnedSlicesAreCopies(t *testing.T) {
	cache := newModelCache(time.Hour, time.Hour, func(context.Context, config.Provider) ([]Model, error) {
		return []Model{{ID: "m1", Label: "M1"}}, nil
	})
	first, err := cache.Models(context.Background(), cacheTestProvider())
	if err != nil {
		t.Fatal(err)
	}
	first[0].ID = "mutated"
	second, err := cache.Models(context.Background(), cacheTestProvider())
	if err != nil {
		t.Fatal(err)
	}
	if second[0].ID != "m1" {
		t.Fatalf("cache corrupted by caller mutation: %+v", second[0])
	}
}

func TestModelCacheWrapsPlainErrors(t *testing.T) {
	cache := newModelCache(time.Hour, time.Hour, func(context.Context, config.Provider) ([]Model, error) {
		return nil, errors.New("plain failure")
	})
	_, err := cache.Models(context.Background(), cacheTestProvider())
	var modelErr *ModelError
	if err == nil || !errors.As(err, &modelErr) || modelErr.Kind != ModelErrUpstream {
		t.Fatalf("err = %v, want wrapped upstream ModelError", err)
	}
}
