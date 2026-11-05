package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/jameynakama/whatnow/internal/integrations"
)

const token = "secret-pat"

// server answers every request with status and body, and records the last request.
func server(t *testing.T, status int, body string) (*httptest.Server, *http.Request) {
	t.Helper()
	got := &http.Request{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = *r.Clone(context.Background())
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func cfg(url string) integrations.Config {
	return integrations.Config{Values: map[string]string{"url": url, "token": token, "jql": "project = FLEX1"}}
}

func pull(t *testing.T, c integrations.Config) integrations.Source {
	t.Helper()
	sources := New(nil).Pull(context.Background(), c, time.Time{}, time.Time{})
	if len(sources) != 1 || sources[0].Label != "Jira" {
		t.Fatalf("sources = %+v", sources)
	}
	return sources[0]
}

const search = `{"total": 63, "issues": [
	{"key": "FLEX1-2", "fields": {"summary": "Unassigned one", "description": null, "assignee": null}},
	{"key": "FLEX1-1", "fields": {"summary": "[quiz] Subscribe to PubSub", "description": "See https://x.test/doc", "assignee": {"name": "me"}}}
]}`

func TestPullOffersTicketsMineFirst(t *testing.T) {
	srv, got := server(t, http.StatusOK, search)
	src := pull(t, cfg(srv.URL+"/jira/"))

	want := []integrations.Ticket{
		{Key: "FLEX1-1", Title: "FLEX1-1 [quiz] Subscribe to PubSub", Notes: "See https://x.test/doc", URL: srv.URL + "/jira/browse/FLEX1-1"},
		{Key: "FLEX1-2", Title: "FLEX1-2 Unassigned one", Notes: "", URL: srv.URL + "/jira/browse/FLEX1-2"},
	}
	if diff := cmp.Diff(want, src.Tickets); diff != "" || src.Err != nil || src.More != 61 {
		t.Errorf("err %v, more %d, tickets (-want +got):\n%s", src.Err, src.More, diff)
	}
	if got.URL.Path != "/jira/rest/api/2/search" || got.Header.Get("Authorization") != "Bearer "+token {
		t.Errorf("request = %s %v", got.URL.Path, got.Header)
	}
	q := got.URL.Query()
	if q.Get("jql") != "project = FLEX1" || q.Get("fields") != "summary,description,assignee" || q.Get("maxResults") != "50" {
		t.Errorf("query = %v", q)
	}
}

func TestPullExplainsWhatWentWrongWithoutTheToken(t *testing.T) {
	bad, _ := json.Marshal(map[string]any{"errorMessages": []string{"Field 'sprnt' does not exist.", "Try again."}})
	cases := map[string]struct {
		status int
		body   string
		want   string
		hint   string
	}{
		"bad token":   {http.StatusUnauthorized, "", "Jira rejected the token", "computer"},
		"bad search":  {http.StatusBadRequest, string(bad), "Field 'sprnt' does not exist. Try again.", ""},
		"server down": {http.StatusInternalServerError, "", "Jira answered 500", ""},
		"login page":  {http.StatusOK, "<html>Sign in</html>", "couldn't read Jira's answer — is the address right?", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := server(t, c.status, c.body)
			src := pull(t, cfg(srv.URL))
			if src.Err == nil || src.Err.Error() != c.want {
				t.Errorf("err = %v, want %q", src.Err, c.want)
			}
			if src.Hint != c.hint {
				t.Errorf("hint = %q, want %q", src.Hint, c.hint)
			}
		})
	}
}

func TestPullRefusesTooBigAnAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, maxBody+1))
	}))
	defer srv.Close()
	src := pull(t, cfg(srv.URL))
	if src.Err == nil || src.Err.Error() != "Jira's response was too large to read" {
		t.Errorf("err = %v", src.Err)
	}
}

func TestPullBlamesTheNetworkWhenJiraIsOutOfReach(t *testing.T) {
	src := pull(t, cfg("http://127.0.0.1:1"))
	if src.Err == nil || src.Err.Error() != "Can't reach Jira — on the VPN?" || strings.Contains(src.Err.Error(), token) || src.Hint != "computer" {
		t.Errorf("source = %+v", src)
	}
}

func TestPullGivesUpOnASlowJira(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	sources := New(&http.Client{Timeout: 50 * time.Millisecond}).Pull(context.Background(), cfg(srv.URL), time.Time{}, time.Time{})
	if err := sources[0].Err; err == nil || err.Error() != "Can't reach Jira — on the VPN?" {
		t.Errorf("err = %v", err)
	}
}

func TestPullSaysWhenTheAddressIsntAURL(t *testing.T) {
	for _, addr := range []string{"intranet.example.com/jira", "https://"} {
		src := pull(t, cfg(addr))
		if src.Err == nil || src.Err.Error() != "that address isn't a URL, such as https://jira.example.com" || src.Hint != "" {
			t.Errorf("%s: err = %v", addr, src.Err)
		}
	}
}
