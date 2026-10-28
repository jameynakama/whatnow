package settings

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jameynakama/whatnow/internal/llm"
	"github.com/jameynakama/whatnow/internal/secrets"
)

// ErrNotConfigured means no provider is chosen, or the chosen one has no key.
var ErrNotConfigured = errors.New("no LLM provider is set up")

// KeySource says where a provider's key comes from. Empty means there is no key.
type KeySource string

const (
	KeyFromKeychain KeySource = "keychain"
	KeyFromEnv      KeySource = "env"
	KeyFromSaved    KeySource = "saved"
)

var envVars = map[Provider]string{Anthropic: "ANTHROPIC_API_KEY", OpenAI: "OPENAI_API_KEY"}

// Keys reads and writes provider API keys in a secrets store.
type Keys struct {
	store  secrets.Store
	source KeySource
	// env lets a provider's usual environment variable stand in for a missing key.
	env bool
}

// LocalKeys keeps keys in the OS keychain, which the environment overrides. The keychain is
// time-limited and cached: a dead or locked Secret Service mustn't hang the app.
func LocalKeys() Keys {
	return Keys{store: secrets.Cached(secrets.WithTimeout(secrets.Keychain{}, 3*time.Second)), source: KeyFromKeychain, env: true}
}

// SealedKeys keeps keys in s alone. The server's environment is never a user's key.
func SealedKeys(s secrets.Store) Keys {
	return Keys{store: s, source: KeyFromSaved}
}

// Key returns a provider's API key from its usual environment variable where that's allowed,
// else the store. The environment wins, the way it does for the CLIs users know, and a set
// variable never touches the store at all: a saved key is shadowed, not deleted, while one is.
func (k Keys) Key(ctx context.Context, p Provider) (string, KeySource, error) {
	if k.env {
		if key := os.Getenv(envVars[p]); key != "" {
			return key, KeyFromEnv, nil
		}
	}
	key, err := k.store.Get(ctx, string(p))
	if err != nil {
		if !k.env {
			return "", "", err
		}
		// An unavailable keychain (no Secret Service, a denied prompt) is no key, not an error.
		log.Printf("read %s key from keychain: %v", p, err)
		return "", "", nil
	}
	if key != "" {
		return key, k.source, nil
	}
	return "", "", nil
}

func (k Keys) Set(ctx context.Context, p Provider, key string) error {
	if !p.Valid() {
		return fmt.Errorf("%w: provider %q", ErrInvalid, p)
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("%w: key is empty", ErrInvalid)
	}
	return k.store.Set(ctx, string(p), key)
}

// Delete removes a provider's key. Deleting a key that isn't there is fine.
func (k Keys) Delete(ctx context.Context, p Provider) error {
	if !p.Valid() {
		return fmt.Errorf("%w: provider %q", ErrInvalid, p)
	}
	return k.store.Delete(ctx, string(p))
}

// Clients is what building a model client needs: where keys live, and the HTTP client the
// provider SDKs send with. A nil HTTP uses the SDK default.
type Clients struct {
	Keys Keys
	HTTP *http.Client
}

// NewClient returns a client for the active provider, along with its model settings.
func NewClient(ctx context.Context, c Clients, s Settings) (llm.Client, ProviderConfig, error) {
	if s.Provider == "" {
		return nil, ProviderConfig{}, ErrNotConfigured
	}
	key, _, err := c.Keys.Key(ctx, s.Provider)
	if err != nil {
		return nil, ProviderConfig{}, err
	}
	if key == "" {
		return nil, ProviderConfig{}, ErrNotConfigured
	}
	cfg := s.Config()
	if s.Provider == OpenAI {
		return llm.NewOpenAI(key, cfg.BaseURL, c.HTTP), cfg, nil
	}
	return llm.NewAnthropic(key, cfg.BaseURL, c.HTTP), cfg, nil
}
