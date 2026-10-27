package secrets

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrUnavailable means a store didn't answer in time, such as an OS keychain whose Secret
// Service is dead or locked.
var ErrUnavailable = errors.New("secrets: the store didn't answer in time")

// WithTimeout bounds store's calls at d, for a store that can hang, as the OS keychain can. A
// call that times out fails with ErrUnavailable; its goroutine may stay blocked in the store,
// which a Cached wrapper outside bounds to once per name.
func WithTimeout(store Store, d time.Duration) Store {
	return &timed{store: store, d: d}
}

type timed struct {
	store Store
	d     time.Duration
}

func (t *timed) Get(ctx context.Context, name string) (string, error) {
	return call(t.d, func() (string, error) { return t.store.Get(ctx, name) })
}

func (t *timed) Set(ctx context.Context, name, value string) error {
	_, err := call(t.d, func() (bool, error) { return false, t.store.Set(ctx, name, value) })
	return err
}

func (t *timed) Delete(ctx context.Context, name string) error {
	_, err := call(t.d, func() (bool, error) { return false, t.store.Delete(ctx, name) })
	return err
}

// call runs f, failing with ErrUnavailable when it takes over d.
func call[T any](d time.Duration, f func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := f()
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-time.After(d):
		var zero T
		return zero, ErrUnavailable
	}
}

// Cached reads store once per name and answers from memory after, for a store whose reads can
// block, as the OS keychain's can. Set and Delete write through. Every name a wrapper serves
// must be written through the same wrapper — the app's secrets name spaces each have one — and
// edits made outside the app show on restart. Failures aren't cached: the next call tries again.
func Cached(store Store) Store {
	return &cached{store: store, have: map[string]string{}}
}

type cached struct {
	store Store
	mu    sync.Mutex
	have  map[string]string
}

func (c *cached) Get(ctx context.Context, name string) (string, error) {
	c.mu.Lock()
	v, ok := c.have[name]
	c.mu.Unlock()
	if ok {
		return v, nil
	}
	v, err := c.store.Get(ctx, name)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.have[name] = v
	c.mu.Unlock()
	return v, nil
}

func (c *cached) Set(ctx context.Context, name, value string) error {
	if err := c.store.Set(ctx, name, value); err != nil {
		return err
	}
	c.mu.Lock()
	c.have[name] = value
	c.mu.Unlock()
	return nil
}

func (c *cached) Delete(ctx context.Context, name string) error {
	if err := c.store.Delete(ctx, name); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.have, name)
	c.mu.Unlock()
	return nil
}
