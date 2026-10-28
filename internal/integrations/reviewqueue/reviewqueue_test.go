package reviewqueue

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/jameynakama/whatnow/internal/integrations"
)

// server answers every request with status and body, and records the last request's path.
func server(t *testing.T, status int, body string) (*httptest.Server, *string) {
	t.Helper()
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &path
}

func cfg(url, minScore string) integrations.Config {
	return integrations.Config{Values: map[string]string{"url": url, "min_score": minScore}}
}

func pull(t *testing.T, c integrations.Config) integrations.Source {
	t.Helper()
	sources := New(nil).Pull(context.Background(), c, time.Time{}, time.Time{})
	if len(sources) != 1 || sources[0].Label != "Review Queue" {
		t.Fatalf("sources = %+v", sources)
	}
	return sources[0]
}

const queue = `{"fetchedAt": "2026-10-15T14:02:11.000Z", "pulls": [
	{"id": "acme/search#1", "url": "https://github.com/acme/search/pull/1", "title": "Cache facet counts",
	 "repository": "acme/search", "author": "alex", "score": 170,
	 "reasons": ["Review requested from you", "Re-review requested after your review"], "estimate": "5–10 min", "rereview": true},
	{"id": "acme/platform#7", "url": "https://github.com/acme/platform/pull/7", "title": "Bump lodash",
	 "repository": "acme/platform", "author": "renovate[bot]", "score": 70,
	 "reasons": ["Assigned maintenance repository", "Waiting for review"], "estimate": "5–10 min", "rereview": false}
]}`

func TestPullOffersTheRankedQueue(t *testing.T) {
	srv, path := server(t, http.StatusOK, queue)
	src := pull(t, cfg(srv.URL+"/", ""))

	want := []integrations.Ticket{
		{
			Key: "acme/search#1", Title: "Review acme/search#1: Cache facet counts",
			Notes: "Score 170 · Review requested from you, Re-review requested after your review · 5–10 min",
			URL:   "https://github.com/acme/search/pull/1", Rereview: true,
		},
		{
			Key: "acme/platform#7", Title: "Review acme/platform#7: Bump lodash",
			Notes: "Score 70 · Assigned maintenance repository, Waiting for review · 5–10 min",
			URL:   "https://github.com/acme/platform/pull/7",
		},
	}
	if diff := cmp.Diff(want, src.Tickets); diff != "" || src.Err != nil || src.More != 0 || *path != "/api/queue" {
		t.Errorf("err %v, more %d, path %q, tickets (-want +got):\n%s", src.Err, src.More, *path, diff)
	}
}

func TestMinScoreCutsTheLowerPulls(t *testing.T) {
	srv, _ := server(t, http.StatusOK, queue)
	for minScore, want := range map[string]int{" 100 ": 1, "70": 2, "-5": 2, "171": 0} {
		if src := pull(t, cfg(srv.URL, minScore)); src.Err != nil || len(src.Tickets) != want {
			t.Errorf("min %q: err %v, %d tickets, want %d", minScore, src.Err, len(src.Tickets), want)
		}
	}
	if src := pull(t, cfg(srv.URL, "high")); src.Err == nil || src.Err.Error() != "minimum score must be a whole number" {
		t.Errorf("min high: err %v", src.Err)
	}
}

func TestABlankMinimumKeepsNegativeScores(t *testing.T) {
	srv, _ := server(t, http.StatusOK, `{"pulls": [{"id": "acme/x#1", "title": "Already has reviewers", "score": -50, "reasons": []}]}`)
	if src := pull(t, cfg(srv.URL, "")); src.Err != nil || len(src.Tickets) != 1 {
		t.Errorf("err %v, tickets %+v", src.Err, src.Tickets)
	}
}

func TestALongQueueIsCappedAndCountsTheRest(t *testing.T) {
	var pulls []string
	for i := range maxResults + 3 {
		pulls = append(pulls, fmt.Sprintf(`{"id": "acme/x#%d", "title": "t", "score": 1, "reasons": []}`, i))
	}
	srv, _ := server(t, http.StatusOK, `{"pulls": [`+strings.Join(pulls, ",")+`]}`)
	src := pull(t, cfg(srv.URL, ""))
	if len(src.Tickets) != maxResults || src.More != 3 || src.Tickets[0].Key != "acme/x#0" || src.Tickets[0].Notes != "Score 1" {
		t.Errorf("%d tickets, more %d, first %+v", len(src.Tickets), src.More, src.Tickets[0])
	}
}

func TestPullErrorsSayWhatToDo(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	cases := map[string]struct {
		url    string
		status int
		body   string
		want   string
	}{
		"not running":      {url: gone.URL, want: "Can't reach Review Queue — is it running?"},
		"not a url":        {url: "localhost:5173", want: "that address isn't a URL, such as http://localhost:5173"},
		"demo source":      {status: http.StatusConflict, body: `{"error": "Set Review Queue's source to GitHub first."}`, want: "Set Review Queue's source to GitHub first."},
		"github failed":    {status: http.StatusBadGateway, body: `{"error": "GitHub rate limit reached."}`, want: "GitHub rate limit reached."},
		"no error message": {status: http.StatusNotFound, body: "Cannot GET /api/queue", want: "Review Queue answered 404"},
		"not json":         {status: http.StatusOK, body: "<!doctype html>", want: "couldn't read Review Queue's answer — is the address right, and is Review Queue up to date?"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if c.url == "" {
				srv, _ := server(t, c.status, c.body)
				c.url = srv.URL
			}
			src := pull(t, cfg(c.url, ""))
			if src.Err == nil || src.Err.Error() != c.want || src.Tickets != nil {
				t.Errorf("err %v, tickets %+v; want %q", src.Err, src.Tickets, c.want)
			}
		})
	}
}

func TestPullRefusesTooBigAnAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, maxBody+1))
	}))
	t.Cleanup(srv.Close)
	src := pull(t, cfg(srv.URL, ""))
	if src.Err == nil || src.Err.Error() != "Review Queue's response was too large to read" {
		t.Errorf("err = %v", src.Err)
	}
}

func TestATimeoutBlamesGitHubNotReviewQueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	sources := New(&http.Client{Timeout: 50 * time.Millisecond}).Pull(context.Background(), cfg(srv.URL, ""), time.Time{}, time.Time{})
	if err := sources[0].Err; err == nil || err.Error() != "Review Queue took over a minute — GitHub may be slow, try again" {
		t.Errorf("err = %v", err)
	}
}
