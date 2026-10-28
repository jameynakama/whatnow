package secrets

import (
	"context"
	"errors"
	"testing"
	"time"
)

// hang is a Store that blocks until released.
type hang chan struct{}

func (h hang) Get(context.Context, string) (string, error) { <-h; return "", nil }
func (h hang) Set(context.Context, string, string) error   { <-h; return nil }
func (h hang) Delete(context.Context, string) error        { <-h; return nil }

func TestWithTimeoutCutsOffAHungStore(t *testing.T) {
	h := hang(make(chan struct{}))
	t.Cleanup(func() { close(h) })
	s := WithTimeout(h, 20*time.Millisecond)
	if _, err := s.Get(context.Background(), "k"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("get: %v", err)
	}
	if err := s.Set(context.Background(), "k", "v"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("set: %v", err)
	}
	if err := s.Delete(context.Background(), "k"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("delete: %v", err)
	}
}

func TestWithTimeoutPassesThroughAQuickStore(t *testing.T) {
	s := WithTimeout(&counting{have: map[string]string{"k": "v"}}, time.Minute)
	if got := mustGet(t, s, "k"); got != "v" {
		t.Errorf("got %q", got)
	}
}

// counting is a Store over a map that counts its reads.
type counting struct {
	have  map[string]string
	reads int
	err   error
}

func (c *counting) Get(_ context.Context, name string) (string, error) {
	c.reads++
	return c.have[name], c.err
}
func (c *counting) Set(_ context.Context, name, value string) error {
	c.have[name] = value
	return nil
}
func (c *counting) Delete(_ context.Context, name string) error {
	delete(c.have, name)
	return nil
}

func TestCachedReadsOnceAndWritesThrough(t *testing.T) {
	under := &counting{have: map[string]string{"k": "v"}}
	c := Cached(under)
	if got := mustGet(t, c, "k"); got != "v" {
		t.Fatalf("got %q", got)
	}
	if got := mustGet(t, c, "k"); got != "v" || under.reads != 1 {
		t.Errorf("second read: %q, %d underlying reads", got, under.reads)
	}
	mustGet(t, c, "nope")
	mustGet(t, c, "nope")
	if under.reads != 2 {
		t.Errorf("a missing name should be cached too: %d reads", under.reads)
	}
	if err := c.Set(context.Background(), "k", "v2"); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, c, "k"); got != "v2" || under.have["k"] != "v2" {
		t.Errorf("after set: cached %q, underlying %q", got, under.have["k"])
	}
	if err := c.Delete(context.Background(), "k"); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, c, "k"); got != "" || under.reads != 3 {
		t.Errorf("after delete: %q, %d underlying reads", got, under.reads)
	}
}

func TestCachedRetriesAFailure(t *testing.T) {
	under := &counting{have: map[string]string{}, err: errors.New("down")}
	c := Cached(under)
	if _, err := c.Get(context.Background(), "k"); err == nil {
		t.Fatal("first read: want an error")
	}
	under.err = nil
	under.have["k"] = "up"
	if got := mustGet(t, c, "k"); got != "up" {
		t.Errorf("retry: got %q", got)
	}
}
