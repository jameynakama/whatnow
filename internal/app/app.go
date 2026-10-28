// Package app builds whatnow's parts and runs their background work. The server and the TUI both
// start from it, so a change made in either is cleaned up and replanned the same way.
package app

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jameynakama/whatnow/internal/agent"
	"github.com/jameynakama/whatnow/internal/cleanup"
	"github.com/jameynakama/whatnow/internal/integrations"
	"github.com/jameynakama/whatnow/internal/integrations/gcal"
	"github.com/jameynakama/whatnow/internal/integrations/jira"
	"github.com/jameynakama/whatnow/internal/integrations/linear"
	"github.com/jameynakama/whatnow/internal/integrations/reviewqueue"
	"github.com/jameynakama/whatnow/internal/llm"
	"github.com/jameynakama/whatnow/internal/outbound"
	"github.com/jameynakama/whatnow/internal/planner"
	"github.com/jameynakama/whatnow/internal/secrets"
	"github.com/jameynakama/whatnow/internal/service"
	"github.com/jameynakama/whatnow/internal/settings"
	"github.com/jameynakama/whatnow/internal/store"
	"github.com/jameynakama/whatnow/internal/stream"
)

// Options says where the database is and how to make the model client. Now and Loc are the
// clock, which tests fix.
type Options struct {
	// DBPath defaults to <user config dir>/whatnow/whatnow.db.
	DBPath    string
	NewClient func(context.Context, settings.Clients, settings.Settings) (llm.Client, settings.ProviderConfig, error)
	Now       func() time.Time
	Loc       *time.Location
	// LinearURL points Linear at another address, such as a fake in an e2e run. Blank is Linear.
	LinearURL string
	// Hosted makes this one user's App on a shared server. Nil is local mode.
	Hosted *Hosted
}

// Hosted is what a hosted App needs beyond local mode. Its secrets are sealed in the user's
// database, its outbound requests can't reach private addresses, it has no Review Queue, and
// its Loc comes from the user's time zone setting, not Options.Loc.
type Hosted struct {
	SealKey [32]byte
}

// App is the running set of parts. Close stops its background work and closes the database.
type App struct {
	DB      *sql.DB
	DBPath  string
	Service *service.Service
	Hub     *stream.Hub
	Cleanup *cleanup.Runner
	Planner *planner.Planner
	Agent   *agent.Agent
	Now     func() time.Time
	Loc     *time.Location
	// Clients is where this app's keys live and what its model clients send with.
	Clients settings.Clients
	// Integrations pull outside work in, such as calendar events, when the user asks.
	Integrations *integrations.Runner

	newClient func(context.Context, settings.Clients, settings.Settings) (llm.Client, settings.ProviderConfig, error)
	stop      context.CancelFunc
	running   sync.WaitGroup
}

// Open opens the database and starts the cleanup runner and the replan watcher.
func Open(o Options) (*App, error) {
	path := o.DBPath
	if path == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(dir, "whatnow", "whatnow.db")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := store.Open(path)
	if err != nil {
		return nil, err
	}

	clients := settings.Clients{Keys: settings.LocalKeys()}
	// The keychain can hang on a dead or locked Secret Service; bound and cache its answers.
	var vault secrets.Store = secrets.Cached(secrets.WithTimeout(secrets.Keychain{}, 3*time.Second))
	pulls := []integrations.Integration{gcal.New(nil), jira.New(nil), linear.NewAt(nil, o.LinearURL), reviewqueue.New(nil)}
	loc := o.Loc
	if o.Hosted != nil {
		sealed := secrets.NewSealed(db, o.Hosted.SealKey)
		vault = sealed
		// A streamed chat reply can outlast any overall timeout, so the model clients get none.
		clients = settings.Clients{Keys: settings.SealedKeys(sealed), HTTP: outbound.Client(0)}
		guarded := outbound.Client(30 * time.Second)
		pulls = []integrations.Integration{gcal.New(guarded), jira.New(guarded), linear.NewAt(guarded, o.LinearURL)}
		loc = SavedLoc(context.Background(), db)
	}
	svc := service.New(db, o.Now)
	hub := stream.NewHub()
	active := func(ctx context.Context) (llm.Client, settings.ProviderConfig, settings.Settings, error) {
		s, err := settings.Load(ctx, db)
		if err != nil {
			return nil, settings.ProviderConfig{}, settings.Settings{}, err
		}
		client, cfg, err := o.NewClient(ctx, clients, s)
		return client, cfg, s, err
	}
	plans := planner.New(svc, hub, active, loc, o.Now)
	a := &App{
		DB:           db,
		DBPath:       path,
		Service:      svc,
		Hub:          hub,
		Cleanup:      cleanup.New(svc, hub, active, loc, o.Now),
		Planner:      plans,
		Agent:        agent.New(svc, hub, plans, loc, o.Now),
		Now:          o.Now,
		Loc:          loc,
		Clients:      clients,
		newClient:    o.NewClient,
		Integrations: integrations.NewRunner(db, svc, hub, loc, o.Now, vault, pulls...),
	}

	ctx, stop := context.WithCancel(context.Background())
	a.stop = stop
	watched, unsubscribe := hub.Subscribe("batch.applied", "batch.undone")
	a.running.Add(2)
	go func() {
		defer a.running.Done()
		a.Cleanup.Run(ctx)
	}()
	go func() {
		defer a.running.Done()
		defer unsubscribe()
		plans.Run(ctx, watched)
	}()
	return a, nil
}

// SavedLoc is the time zone saved in a hosted user's settings, or UTC when none is.
func SavedLoc(ctx context.Context, db *sql.DB) *time.Location {
	if saved, err := settings.Load(ctx, db); err == nil && saved.TimeZone != "" {
		if l, err := time.LoadLocation(saved.TimeZone); err == nil {
			return l
		}
	}
	return time.UTC
}

// NewClient builds the model client for s from this app's keys and HTTP client.
func (a *App) NewClient(ctx context.Context, s settings.Settings) (llm.Client, settings.ProviderConfig, error) {
	return a.newClient(ctx, a.Clients, s)
}

// Close stops the background work, waits for it, and closes the database.
func (a *App) Close() error {
	a.stop()
	a.running.Wait()
	return a.DB.Close()
}
