package linear

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/jameynakama/whatnow/internal/integrations"
)

const key = "lin_api_secret"

const uuidView = "86e915b6-ac3c-47ec-b97c-ffc4a1fe30e7"

// request is what the fake Linear last received.
type request struct {
	Header http.Header
	Query  string
	Vars   map[string]any
}

// server answers every request with status and body, and records the last request.
func server(t *testing.T, status int, body string) (*httptest.Server, *request) {
	t.Helper()
	got := &request{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var sent struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.Unmarshal(raw, &sent)
		*got = request{Header: r.Header.Clone(), Query: sent.Query, Vars: sent.Variables}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func cfg(view string) integrations.Config {
	return integrations.Config{Values: map[string]string{"api_key": key, "view": view}}
}

func pull(t *testing.T, url string, c integrations.Config) integrations.Source {
	t.Helper()
	sources := NewAt(nil, url).Pull(context.Background(), c, time.Time{}, time.Time{})
	if len(sources) != 1 || sources[0].Label != "Linear" {
		t.Fatalf("sources = %+v", sources)
	}
	return sources[0]
}

const mine = `{"data": {"viewer": {
	"id": "u1",
	"shown": {"nodes": [
		{"identifier": "ENG-3", "title": "A teammate's", "description": null, "url": "https://linear.app/acme/issue/ENG-3", "assignee": {"id": "u2"}},
		{"identifier": "ENG-2", "title": "Unassigned one", "description": null, "url": "https://linear.app/acme/issue/ENG-2", "assignee": null},
		{"identifier": "ENG-1", "title": "Fix the **login**", "description": "See [doc](https://x.test/doc)", "url": "https://linear.app/acme/issue/ENG-1", "assignee": {"id": "u1"}}
	]},
	"rest": {"nodes": [{"id": "a"}, {"id": "b"}, {"id": "c"}, {"id": "d"}, {"id": "e"}]}
}}}`

func TestPullOffersAssignedIssuesMineFirst(t *testing.T) {
	srv, got := server(t, http.StatusOK, mine)
	src := pull(t, srv.URL, cfg(""))

	want := []integrations.Ticket{
		{Key: "ENG-1", Title: "ENG-1 Fix the **login**", Notes: "See [doc](https://x.test/doc)", URL: "https://linear.app/acme/issue/ENG-1"},
		{Key: "ENG-3", Title: "ENG-3 A teammate's", Notes: "", URL: "https://linear.app/acme/issue/ENG-3"},
		{Key: "ENG-2", Title: "ENG-2 Unassigned one", Notes: "", URL: "https://linear.app/acme/issue/ENG-2"},
	}
	if diff := cmp.Diff(want, src.Tickets); diff != "" || src.Err != nil || src.More != 2 {
		t.Errorf("err %v, more %d, tickets (-want +got):\n%s", src.Err, src.More, diff)
	}
	if got.Header.Get("Authorization") != key || got.Header.Get("Content-Type") != "application/json" {
		t.Errorf("headers = %v", got.Header)
	}
	if !strings.Contains(got.Query, "assignedIssues") || got.Vars != nil {
		t.Errorf("query = %q, variables = %v", got.Query, got.Vars)
	}
}

func TestPullAsksForAViewByItsSlugID(t *testing.T) {
	const view = `{"data": {"viewer": {"id": "u1"}, "customView": {"shown": {"nodes": [
		{"identifier": "BE-9", "title": "Late", "description": "", "url": "https://linear.app/acme/issue/BE-9", "assignee": null}
	]}, "rest": {"nodes": [{"id": "a"}]}}}}`
	addrs := map[string]string{
		"https://linear.app/acme/view/my-bugs-0a1b2c3d4e5f":    "0a1b2c3d4e5f",
		"https://linear.app/acme/view/0a1b2c3d4e5f":            "0a1b2c3d4e5f",
		"  https://linear.app/acme/view/my-bugs-0a1b2c3d4e5f/": "0a1b2c3d4e5f",
		"https://linear.app/acme/view/" + uuidView:             uuidView,
	}
	for addr, id := range addrs {
		t.Run(addr, func(t *testing.T) {
			srv, got := server(t, http.StatusOK, view)
			src := pull(t, srv.URL, cfg(addr))
			if src.Err != nil || len(src.Tickets) != 1 || src.Tickets[0].Key != "BE-9" || src.More != 0 {
				t.Errorf("source = %+v", src)
			}
			if !strings.Contains(got.Query, "customView") || !cmp.Equal(got.Vars, map[string]any{"id": id}) {
				t.Errorf("query = %q, variables = %v", got.Query, got.Vars)
			}
		})
	}
}

func TestPullKeepsUnassignedIssuesInOrderBehindYours(t *testing.T) {
	const body = `{"data": {"viewer": {"id": "u1"}, "customView": {"shown": {"nodes": [
		{"identifier": "A-1", "title": "t", "assignee": null},
		{"identifier": "A-2", "title": "t", "assignee": {"id": "u2"}},
		{"identifier": "A-3", "title": "t", "assignee": null},
		{"identifier": "A-4", "title": "t", "assignee": {"id": "u1"}}
	]}, "rest": {"nodes": []}}}}`
	srv, _ := server(t, http.StatusOK, body)
	var keys []string
	for _, tk := range pull(t, srv.URL, cfg("https://linear.app/acme/view/my-bugs-0a1b2c3d4e5f")).Tickets {
		keys = append(keys, tk.Key)
	}
	if diff := cmp.Diff([]string{"A-4", "A-1", "A-2", "A-3"}, keys); diff != "" {
		t.Errorf("order (-want +got):\n%s", diff)
	}
}

func TestPullAppliesTheFilterInAViewsAddress(t *testing.T) {
	const view = `{"data": {"customView": {"shown": {"nodes": []}, "rest": {"nodes": []}}}}`
	const filter = `{"and":[{"assignee":{"or":[{"id":{"in":["86e915b6-ac3c-47ec-b97c-ffc4a1fe30e7"]}}]}}]}`
	const base = "https://linear.app/realm/view/current-cycle-top-prios-open-4a6ba4535d33?filter="
	for _, c := range []struct{ name, address, want string }{
		{"url-safe unpadded", base + base64.RawURLEncoding.EncodeToString([]byte(filter)), filter},
		{"standard padded", base + url.QueryEscape(base64.StdEncoding.EncodeToString([]byte(`{"a":"~~~?"}`))), `{"a":"~~~?"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			want := c.want
			srv, got := server(t, http.StatusOK, view)
			if src := pull(t, srv.URL, cfg(c.address)); src.Err != nil {
				t.Fatalf("err = %v", src.Err)
			}
			wantVars := `{"filter":` + want + `,"id":"4a6ba4535d33"}`
			gotVars, _ := json.Marshal(got.Vars)
			if !strings.Contains(got.Query, "customView") || string(gotVars) != wantVars {
				t.Errorf("query = %q, variables = %s", got.Query, gotVars)
			}
		})
	}
}

func TestPullKeepsTheViewQueryWhenTheFilterIsEmpty(t *testing.T) {
	srv, got := server(t, http.StatusOK, `{"data": {"customView": {"shown": {"nodes": []}, "rest": {"nodes": []}}}}`)
	pull(t, srv.URL, cfg("https://linear.app/acme/view/my-bugs-0a1b2c3d4e5f?filter="))
	if got.Query != viewQuery || !cmp.Equal(got.Vars, map[string]any{"id": "0a1b2c3d4e5f"}) {
		t.Errorf("query = %q, variables = %v", got.Query, got.Vars)
	}
}

func TestPullRefusesAFilterThatIsntAnObject(t *testing.T) {
	for name, encoded := range map[string]string{
		"not base64": "!!!",
		"not json":   base64.RawURLEncoding.EncodeToString([]byte("nope")),
		"an array":   base64.RawURLEncoding.EncodeToString([]byte(`[1]`)),
		"null":       base64.RawURLEncoding.EncodeToString([]byte(`null`)),
	} {
		t.Run(name, func(t *testing.T) {
			srv, got := server(t, http.StatusOK, mine)
			src := pull(t, srv.URL, cfg("https://linear.app/acme/view/my-bugs-0a1b?filter="+encoded))
			if src.Err == nil || src.Err.Error() != "that isn't a Linear view's address" || got.Query != "" {
				t.Errorf("err = %v, asked %q", src.Err, got.Query)
			}
		})
	}
}

func TestPullExplainsWhatWentWrongWithoutTheKey(t *testing.T) {
	auth := `{"errors":[{"message":"Authentication required, not authenticated","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`
	missing := `{"errors":[{"message":"Entity not found: CustomView"},{"message":"Try again."}]}`
	gone := "https://linear.app/acme/view/gone-0a1b2c"
	cases := map[string]struct {
		status int
		body   string
		view   string
		want   string
	}{
		"bad key":               {http.StatusUnauthorized, auth, "", "Linear rejected the API key"},
		"bad key in a 200":      {http.StatusOK, auth, "", "Linear rejected the API key"},
		"bad key on a 400":      {http.StatusBadRequest, auth, "", "Linear rejected the API key"},
		"missing view":          {http.StatusOK, missing, gone, "Entity not found: CustomView Try again."},
		"missing view on a 400": {http.StatusBadRequest, missing, gone, "Entity not found: CustomView Try again."},
		"unreadable 400":        {http.StatusBadRequest, "nope", "", "Linear answered 400"},
		"server down":           {http.StatusInternalServerError, "", "", "Linear answered 500"},
		"not json":              {http.StatusOK, "<html>Sign in</html>", "", "couldn't read Linear's answer"},
		"no viewer":             {http.StatusOK, `{"data": {"viewer": null}}`, "", "couldn't read Linear's answer"},
		"no view":               {http.StatusOK, `{"data": {"customView": null}}`, gone, "couldn't read Linear's answer"},
		"wrong data":            {http.StatusOK, `{"data": {"customView": {"shown": {"nodes": []}, "rest": {"nodes": []}}}}`, "", "couldn't read Linear's answer"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := server(t, c.status, c.body)
			src := pull(t, srv.URL, cfg(c.view))
			if src.Err == nil || src.Err.Error() != c.want || strings.Contains(src.Err.Error(), key) || src.Hint != "" {
				t.Errorf("err = %v, want %q", src.Err, c.want)
			}
		})
	}
}

func TestPullRefusesTooBigAnAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, maxBody+1))
	}))
	t.Cleanup(srv.Close)
	src := pull(t, srv.URL, cfg(""))
	if src.Err == nil || src.Err.Error() != "Linear's response was too large to read" {
		t.Errorf("err = %v", src.Err)
	}
}

func TestPullBlamesTheNetworkWhenLinearIsOutOfReach(t *testing.T) {
	src := pull(t, "http://127.0.0.1:1", cfg(""))
	if src.Err == nil || src.Err.Error() != "Can't reach Linear" || strings.Contains(src.Err.Error(), key) {
		t.Errorf("source = %+v", src)
	}
}

func TestPullGivesUpOnASlowLinear(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reading the body lets the server notice the client hung up.
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer srv.Close()
	sources := NewAt(&http.Client{Timeout: 50 * time.Millisecond}, srv.URL).Pull(context.Background(), cfg(""), time.Time{}, time.Time{})
	if err := sources[0].Err; err == nil || err.Error() != "Can't reach Linear" {
		t.Errorf("err = %v", err)
	}
}

func TestPullRefusesAnAddressThatIsntAView(t *testing.T) {
	for _, addr := range []string{
		"my-bugs", "http://linear.app/acme/view/my-bugs-0a1b", "https://example.com/acme/view/my-bugs-0a1b",
		"https://linear.app/acme/issue/ENG-1", "https://linear.app/acme/view/", "https://linear.app/acme/view/my-bugs-0a1b/extra",
		"https://linear.app/acme/view/my-bugs-",
	} {
		t.Run(addr, func(t *testing.T) {
			srv, got := server(t, http.StatusOK, mine)
			src := pull(t, srv.URL, cfg(addr))
			if src.Err == nil || src.Err.Error() != "that isn't a Linear view's address" || got.Query != "" {
				t.Errorf("err = %v, asked %q", src.Err, got.Query)
			}
		})
	}
}
