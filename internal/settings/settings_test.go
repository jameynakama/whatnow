package settings_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/zalando/go-keyring"

	"github.com/jameynakama/whatnow/internal/llm"
	"github.com/jameynakama/whatnow/internal/settings"
	"github.com/jameynakama/whatnow/internal/store"
)

func TestLoadDefaultsThenSaveRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	got, err := settings.Load(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(settings.Defaults(), got); diff != "" {
		t.Errorf("fresh load differs from defaults:\n%s", diff)
	}

	want := settings.Defaults()
	want.Provider = settings.Anthropic
	want.Style = "gentle, like Thich Nhat Hanh"
	want.AboutMe = "Code review drains me. Writing is easy."
	if err := settings.Save(ctx, db, want); err != nil {
		t.Fatal(err)
	}
	got, err = settings.Load(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("saved settings differ (-want +got):\n%s", diff)
	}
}

func TestLoadFillsFieldsAddedSinceTheSave(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Saved before nudges, the alert lead time, and onboarding existed.
	_, err = db.Exec(`INSERT INTO settings (id, body) VALUES (1, '{"provider":"anthropic","end_of_day":"18:00"}')`)
	if err != nil {
		t.Fatal(err)
	}

	got, err := settings.Load(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	want := settings.Defaults()
	want.Provider = settings.Anthropic
	want.EndOfDay = "18:00"
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("old settings didn't pick up the new defaults (-want +got):\n%s", diff)
	}
}

func TestValidate(t *testing.T) {
	valid := settings.Defaults()
	valid.Provider = settings.Anthropic
	cases := map[string]func(*settings.Settings){
		"unknown provider": func(s *settings.Settings) { s.Provider = "gemini" },
		"openai without models": func(s *settings.Settings) {
			s.Provider = settings.OpenAI
			s.OpenAI = settings.ProviderConfig{}
		},
		"base URL without scheme":       func(s *settings.Settings) { s.OpenAI.BaseURL = "localhost:11434" },
		"style over 500 characters":     func(s *settings.Settings) { s.Style = strings.Repeat("é", 501) },
		"about me over 1000 characters": func(s *settings.Settings) { s.AboutMe = strings.Repeat("🧠", 1001) },
		"end of day not HH:MM":          func(s *settings.Settings) { s.EndOfDay = "5pm" },
		"no alert lead time":            func(s *settings.Settings) { s.AlertLeadMinutes = 0 },
		"alert lead over an hour":       func(s *settings.Settings) { s.AlertLeadMinutes = 61 },
		"unknown theme":                 func(s *settings.Settings) { s.Theme = "geocities" },
		"allowed host with a port":      func(s *settings.Settings) { s.AllowedHosts = []string{"mac.example.ts.net:443"} },
		"allowed host with a scheme":    func(s *settings.Settings) { s.AllowedHosts = []string{"https://mac.example.ts.net"} },
		"blank allowed host":            func(s *settings.Settings) { s.AllowedHosts = []string{" "} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := valid
			mutate(&s)
			if err := s.Validate(); !errors.Is(err, settings.ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
		})
	}

	atLimit := valid
	atLimit.Style = strings.Repeat("é", 500) // 1000 bytes, 500 characters
	atLimit.OpenAI.BaseURL = "http://localhost:11434/v1"
	atLimit.AlertLeadMinutes = 60
	atLimit.Theme = settings.ThemeVaporwave
	atLimit.AllowedHosts = []string{"mac.example.ts.net", "100.64.0.1"}
	if err := atLimit.Validate(); err != nil {
		t.Errorf("settings at the limits rejected: %v", err)
	}
}

func TestKeysPreferEnvOverKeychain(t *testing.T) {
	ctx := context.Background()
	keyring.MockInit()
	t.Setenv("ANTHROPIC_API_KEY", "from-env")

	key, source, err := settings.LocalKeys().Key(ctx, settings.Anthropic)
	if err != nil || key != "from-env" || source != settings.KeyFromEnv {
		t.Fatalf("env: %q %q %v", key, source, err)
	}

	// A saved key is shadowed, not lost, while the variable is set.
	if err := settings.LocalKeys().Set(ctx, settings.Anthropic, "  from-keychain \n"); err != nil {
		t.Fatal(err)
	}
	key, source, err = settings.LocalKeys().Key(ctx, settings.Anthropic)
	if err != nil || key != "from-env" || source != settings.KeyFromEnv {
		t.Fatalf("env over keychain: %q %q %v", key, source, err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	key, source, err = settings.LocalKeys().Key(ctx, settings.Anthropic)
	if err != nil || key != "from-keychain" || source != settings.KeyFromKeychain {
		t.Fatalf("keychain: %q %q %v", key, source, err)
	}

	if err := settings.LocalKeys().Delete(ctx, settings.Anthropic); err != nil {
		t.Fatal(err)
	}
	if err := settings.LocalKeys().Delete(ctx, settings.Anthropic); err != nil {
		t.Errorf("deleting a missing key: %v", err)
	}
	if _, source, _ := settings.LocalKeys().Key(ctx, settings.Anthropic); source != "" {
		t.Errorf("after delete, source = %q, want none", source)
	}
	if err := settings.LocalKeys().Set(ctx, settings.Anthropic, "   "); !errors.Is(err, settings.ErrInvalid) {
		t.Errorf("blank key: err = %v, want ErrInvalid", err)
	}
}

func TestNewClientNeedsProviderAndKey(t *testing.T) {
	ctx := context.Background()
	keyring.MockInit()
	t.Setenv("OPENAI_API_KEY", "")
	clients := settings.Clients{Keys: settings.LocalKeys()}
	s := settings.Defaults()

	if _, _, err := settings.NewClient(ctx, clients, s); !errors.Is(err, settings.ErrNotConfigured) {
		t.Errorf("no provider: err = %v, want ErrNotConfigured", err)
	}
	s.Provider = settings.OpenAI
	s.OpenAI = settings.ProviderConfig{MainModel: "big", LightModel: "small"}
	if _, _, err := settings.NewClient(ctx, clients, s); !errors.Is(err, settings.ErrNotConfigured) {
		t.Errorf("no key: err = %v, want ErrNotConfigured", err)
	}
	// Written through the same Keys, so its cache sees the write.
	if err := clients.Keys.Set(ctx, settings.OpenAI, "sk-test"); err != nil {
		t.Fatal(err)
	}
	client, cfg, err := settings.NewClient(ctx, clients, s)
	if err != nil || client == nil || cfg.LightModel != "small" {
		t.Errorf("configured: client %v, cfg %+v, err %v", client, cfg, err)
	}
}

func TestKeychainFailureFallsBackToEnv(t *testing.T) {
	ctx := context.Background()
	keyring.MockInitWithError(errors.New("no secret service"))
	t.Setenv("ANTHROPIC_API_KEY", "from-env")

	key, source, err := settings.LocalKeys().Key(ctx, settings.Anthropic)
	if err != nil || key != "from-env" || source != settings.KeyFromEnv {
		t.Errorf("got %q %q %v, want the env key", key, source, err)
	}
}

func TestKeychainFailureIsNoKeyNotAnError(t *testing.T) {
	ctx := context.Background()
	keyring.MockInitWithError(errors.New("no secret service"))
	t.Setenv("ANTHROPIC_API_KEY", "")

	key, source, err := settings.LocalKeys().Key(ctx, settings.Anthropic)
	if err != nil || key != "" || source != "" {
		t.Errorf("got %q %q %v, want nothing", key, source, err)
	}
}

// memStore is a secrets.Store over a map.
type memStore map[string]string

func (m memStore) Get(_ context.Context, name string) (string, error) { return m[name], nil }
func (m memStore) Set(_ context.Context, name, value string) error    { m[name] = value; return nil }
func (m memStore) Delete(_ context.Context, name string) error        { delete(m, name); return nil }

func TestSealedKeysIgnoreEnv(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ANTHROPIC_API_KEY", "from-env")
	keys := settings.SealedKeys(memStore{})

	if key, source, err := keys.Key(ctx, settings.Anthropic); err != nil || key != "" || source != "" {
		t.Fatalf("before Set: %q %q %v, want nothing", key, source, err)
	}
	if err := keys.Set(ctx, settings.Anthropic, "sk-saved"); err != nil {
		t.Fatal(err)
	}
	if key, source, err := keys.Key(ctx, settings.Anthropic); err != nil || key != "sk-saved" || source != settings.KeyFromSaved {
		t.Errorf("after Set: %q %q %v, want the saved key", key, source, err)
	}
}

func TestLocalKeysFallBackToEnv(t *testing.T) {
	keyring.MockInit()
	t.Setenv("ANTHROPIC_API_KEY", "from-env")

	key, source, err := settings.LocalKeys().Key(context.Background(), settings.Anthropic)
	if err != nil || key != "from-env" || source != settings.KeyFromEnv {
		t.Errorf("got %q %q %v, want the env key", key, source, err)
	}
}

func TestNewClientUsesHTTPClient(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(api.Close)
	counter := &countingTransport{}
	keys := settings.SealedKeys(memStore{"anthropic": "sk-test"})
	s := settings.Defaults()
	s.Provider = settings.Anthropic
	s.Anthropic.BaseURL = api.URL

	client, _, err := settings.NewClient(context.Background(), settings.Clients{Keys: keys, HTTP: &http.Client{Transport: counter}}, s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Complete(context.Background(), llm.Request{Model: "m", User: "hi", MaxTokens: 16}); err != nil {
		t.Fatal(err)
	}
	if counter.requests != 1 {
		t.Errorf("requests through the supplied client = %d, want 1", counter.requests)
	}
}

type countingTransport struct{ requests int }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.requests++
	return http.DefaultTransport.RoundTrip(r)
}

func TestSettingsSavedBeforeEndOfDayExistedGetTheDefault(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO settings (id, body) VALUES (1, '{"provider":"anthropic","style":"terse"}')`); err != nil {
		t.Fatal(err)
	}

	got, err := settings.Load(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if got.EndOfDay != "17:30" || got.Style != "terse" {
		t.Errorf("loaded %+v", got)
	}
}

func TestValidateTimeZone(t *testing.T) {
	for zone, ok := range map[string]bool{"America/Chicago": true, "": true, "Mars/Olympus": false} {
		s := settings.Defaults()
		s.TimeZone = zone
		err := s.Validate()
		if ok && err != nil {
			t.Errorf("%q: %v", zone, err)
		}
		if !ok && !errors.Is(err, settings.ErrInvalid) {
			t.Errorf("%q: err = %v, want ErrInvalid", zone, err)
		}
	}
}
