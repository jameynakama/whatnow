// Command whatnow runs the terminal companion, which serves the web UI too, or the server alone.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jameynakama/whatnow/internal/app"
	"github.com/jameynakama/whatnow/internal/host"
	"github.com/jameynakama/whatnow/internal/httpapi"
	"github.com/jameynakama/whatnow/internal/secrets"
	"github.com/jameynakama/whatnow/internal/settings"
	"github.com/jameynakama/whatnow/internal/tui"
	"github.com/jameynakama/whatnow/internal/web"
)

// Version is stamped in at release with -ldflags "-X main.Version=...".
var Version = "dev"

// newClient makes the model client. The e2e build swaps in the fake.
var newClient = settings.NewClient

// linearURL points Linear at a fake. The e2e build sets it.
var linearURL string

const defaultAddr = "127.0.0.1:7777"

const usage = "usage: whatnow [-addr 127.0.0.1:7777] [-db PATH]        the terminal view, serving the web UI if nothing else is\n       whatnow serve [-addr 127.0.0.1:7777] [-db PATH]  the web UI only\n       whatnow tui [-addr 127.0.0.1:7777] [-db PATH]    same as bare whatnow\n       whatnow host -data DIR -public-url https://HOST [-addr 127.0.0.1:8080] [-max-users 100]  hosted, for many Google-signed-in users\n       whatnow jira setup|push                     send Jira tickets from this computer to hosted whatnow\n       whatnow version"

func main() {
	cmd, args := command(os.Args[1:])
	flags := flag.NewFlagSet(cmd, flag.ExitOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	dbPath := flags.String("db", "", "database path (default <user config dir>/whatnow/whatnow.db)")
	addr := flags.String("addr", defaultAddr, "listen address; keep it on loopback")
	var err error
	switch cmd {
	case "serve":
		flags.Parse(args)
		err = serve(*addr, *dbPath)
	case "tui":
		flags.Parse(args)
		err = runTUI(*addr, *dbPath)
	case "host":
		err = runHost(args, os.Getenv)
	case "jira":
		err = jiraCommand(args)
	case "version":
		fmt.Println(Version)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func jiraCommand(args []string) error {
	dir, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	// The keychain can hang on a dead or locked Secret Service; bound and cache its answers.
	vault := secrets.Cached(secrets.WithTimeout(secrets.Keychain{}, 3*time.Second))
	return runJira(context.Background(), args, os.Stdin, os.Stdout, filepath.Join(dir, "whatnow"), vault, nil)
}

// command splits off the subcommand. With none, or only flags, it's the TUI.
func command(args []string) (string, []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "tui", args
	}
	return args[0], args[1:]
}

func open(dbPath string) (*app.App, error) {
	now := time.Now
	if path := os.Getenv("WHATNOW_DEMO_CLOCK"); path != "" {
		now = demoClock(path)
	}
	return app.Open(app.Options{DBPath: dbPath, NewClient: newClient, LinearURL: linearURL, Now: now, Loc: time.Local})
}

func serve(addr, dbPath string) error {
	a, err := open(dbPath)
	if err != nil {
		return err
	}
	defer a.Close()
	log.Printf("whatnow listening on http://%s (db %s)", addr, a.DBPath)
	return newServer(a, addr).ListenAndServe()
}

func newServer(a *app.App, addr string) *http.Server {
	// No WriteTimeout: it would cut off the long-lived SSE stream.
	return &http.Server{Addr: addr, Handler: apiHandler(a, false), ReadHeaderTimeout: 5 * time.Second}
}

func apiHandler(a *app.App, hosted bool) http.Handler {
	return httpapi.New(httpapi.Deps{
		Service:      a.Service,
		Hub:          a.Hub,
		DB:           a.DB,
		Loc:          a.Loc,
		Now:          a.Now,
		OnCapture:    a.Cleanup.Enqueue,
		Clean:        a.Cleanup.Clean,
		NewClient:    a.NewClient,
		Keys:         a.Clients.Keys,
		Planner:      a.Planner,
		Agent:        a.Agent,
		Integrations: a.Integrations,
		Static:       web.Handler(),
		Hosted:       hosted,
	})
}

// listenUnlessTaken listens on addr, or returns a nil listener when something already holds it.
func listenUnlessTaken(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if errors.Is(err, syscall.EADDRINUSE) {
		return nil, nil
	}
	return ln, err
}

func runTUI(addr, dbPath string) error {
	a, err := open(dbPath)
	if err != nil {
		return err
	}
	defer a.Close()
	// Background work logs as it goes. Under the TUI, those lines would land on the screen.
	logs, err := tea.LogToFile(filepath.Join(filepath.Dir(a.DBPath), "tui.log"), "")
	if err != nil {
		return err
	}
	defer logs.Close()

	// With no server running, the TUI serves the web UI itself, so the address it shows works. Sharing
	// one process also shares the hub, so each side sees the other's changes live. When a server
	// already holds the address, the TUI points at it instead.
	ln, err := listenUnlessTaken(addr)
	if err != nil {
		return err
	}
	if ln != nil {
		srv := newServer(a, addr)
		go func() {
			if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				log.Printf("web server stopped: %v", err)
			}
		}()
		defer srv.Close()
		log.Printf("whatnow listening on http://%s (db %s)", addr, a.DBPath)
	}
	return tui.Run(a, "http://"+addr)
}

// hostIdle is how long a user's App stays open with no requests.
const hostIdle = 15 * time.Minute

type hostOptions struct {
	addr, dataDir          string
	publicURL              *url.URL
	maxUsers               int
	clientID, clientSecret string
	sealKey                [32]byte
}

// redirectURL is where Google sends the user back: the public origin, whatever path it was given.
func (o hostOptions) redirectURL() string {
	return (&url.URL{Scheme: o.publicURL.Scheme, Host: o.publicURL.Host, Path: "/auth/google/callback"}).String()
}

// hostConfig reads the host flags, and the secrets from the environment, which never take flags.
func hostConfig(args []string, getenv func(string) string) (hostOptions, error) {
	var o hostOptions
	flags := flag.NewFlagSet("host", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	flags.StringVar(&o.addr, "addr", "127.0.0.1:8080", "listen address; put a TLS-terminating proxy in front")
	flags.StringVar(&o.dataDir, "data", "", "data directory for the accounts database and each user's database")
	publicURL := flags.String("public-url", "", "the https URL users reach this server at")
	flags.IntVar(&o.maxUsers, "max-users", 100, "most users who may sign up")
	if err := flags.Parse(args); err != nil {
		return o, err
	}
	if o.dataDir == "" {
		return o, errors.New("-data is required")
	}
	if *publicURL == "" {
		return o, errors.New("-public-url is required")
	}
	u, err := url.Parse(*publicURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return o, fmt.Errorf("-public-url must be an https URL, got %q", *publicURL)
	}
	// Browsers send a lowercase Origin, which writes are compared against.
	u.Host = strings.ToLower(u.Host)
	o.publicURL = u
	for name, dst := range map[string]*string{
		"WHATNOW_GOOGLE_CLIENT_ID":     &o.clientID,
		"WHATNOW_GOOGLE_CLIENT_SECRET": &o.clientSecret,
	} {
		if *dst = getenv(name); *dst == "" {
			return o, fmt.Errorf("%s is required", name)
		}
	}
	seal := getenv("WHATNOW_SEAL_KEY")
	if seal == "" {
		return o, errors.New("WHATNOW_SEAL_KEY is required")
	}
	if o.sealKey, err = secrets.ParseKey(seal); err != nil {
		return o, fmt.Errorf("WHATNOW_SEAL_KEY: %w", err)
	}
	return o, nil
}

// refuseModelEnvironment refuses to host while any ANTHROPIC_ or OPENAI_ variable is set. The
// model SDKs read those as defaults, so a server's own key, base URL, or headers could otherwise
// ride along on a user's requests to the provider they chose.
func refuseModelEnvironment(environ []string) error {
	var set []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "ANTHROPIC_") || strings.HasPrefix(name, "OPENAI_") {
			set = append(set, name)
		}
	}
	if len(set) == 0 {
		return nil
	}
	slices.Sort(set)
	return fmt.Errorf("unset %s: hosted users bring their own model settings", strings.Join(set, ", "))
}

func runHost(args []string, getenv func(string) string) error {
	o, err := hostConfig(args, getenv)
	if err != nil {
		return err
	}
	if err := refuseModelEnvironment(os.Environ()); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(filepath.Join(o.dataDir, "users"), 0o700); err != nil {
		return err
	}
	accounts, err := host.OpenAccounts(filepath.Join(o.dataDir, "accounts.db"), time.Now)
	if err != nil {
		return err
	}
	defer accounts.Close()
	google, err := host.NewGoogle(ctx, o.clientID, o.clientSecret, o.redirectURL())
	if err != nil {
		return err
	}
	registry := host.NewRegistry(o.dataDir, func(_, dbPath string) (*app.App, http.Handler, error) {
		a, err := app.Open(app.Options{DBPath: dbPath, NewClient: newClient, LinearURL: linearURL, Now: time.Now, Loc: time.UTC, Hosted: &app.Hosted{SealKey: o.sealKey}})
		if err != nil {
			return nil, nil, err
		}
		return a, apiHandler(a, true), nil
	}, hostIdle, time.Now)
	go registry.Run(ctx)

	// Shutdown doesn't cancel requests, so without this an open event stream would hold it up until
	// the deadline. Cancelling every request's context ends the streams.
	requests, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	srv := &http.Server{
		Addr:              o.addr,
		Handler:           host.New(host.Config{PublicURL: o.publicURL, MaxUsers: o.maxUsers, Provider: google, Static: web.Handler()}, accounts, registry),
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return requests },
	}
	srv.RegisterOnShutdown(cancelRequests)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	log.Printf("whatnow hosting on http://%s for %s (data %s)", o.addr, o.publicURL, o.dataDir)

	select {
	case err = <-serveErr:
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err = srv.Shutdown(shutdown); errors.Is(err, context.DeadlineExceeded) {
			log.Printf("requests still running after 10s; closing them")
			err = srv.Close()
		}
	}
	// The registry's Close isn't terminal, so it waits until the server takes no more requests.
	registry.Close()
	return err
}
