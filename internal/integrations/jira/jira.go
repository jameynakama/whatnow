// Package jira offers tickets from a Jira Data Center search, through a Personal Access Token.
package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jameynakama/whatnow/internal/integrations"
)

// maxResults caps a pull. A pick list longer than this means the search is too broad.
const maxResults = 50

// maxBody bounds the search response. Fifty summaries and descriptions is generous; past the
// cap the address isn't serving Jira answers, and a hosted server shouldn't buffer them.
const maxBody = 8 << 20

var (
	errUnreachable = errors.New("Can't reach Jira — on the VPN?")
	errRejected    = errors.New("Jira rejected the token")
)

// Jira is the Jira integration.
type Jira struct {
	client *http.Client
}

// New returns the integration. A nil client means one that gives up after 30 seconds.
func New(client *http.Client) *Jira {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Jira{client: client}
}

func (*Jira) ID() string                { return "jira" }
func (*Jira) Name() string              { return "Jira" }
func (*Jira) Pulls() integrations.Pulls { return integrations.PullsTickets }
func (*Jira) Pushable()                 {}

func (*Jira) Fields() []integrations.Field {
	return []integrations.Field{
		{Key: "url", Label: "Address", Kind: integrations.Text, Required: true, Help: "Your Jira's address, such as https://jira.example.com"},
		{Key: "token", Label: "Personal access token", Kind: integrations.Secret, Required: true, Help: "In Jira: your profile → Personal Access Tokens"},
		{Key: "jql", Label: "Search (JQL)", Kind: integrations.Text, Required: true, Help: "Such as: sprint in openSprints() AND statusCategory != Done AND assignee = currentUser()"},
	}
}

// Pull runs the search. The window is ignored: a ticket isn't for a day.
func (j *Jira) Pull(ctx context.Context, cfg integrations.Config, _, _ time.Time) []integrations.Source {
	src := integrations.Source{Label: "Jira"}
	src.Tickets, src.More, src.Err = j.search(ctx, cfg)
	// A Jira behind a VPN looks unreachable from a server outside it, or its network's edge turns
	// the server away before Jira sees the token.
	if errors.Is(src.Err, errUnreachable) || errors.Is(src.Err, errRejected) {
		src.Hint = "computer"
	}
	return []integrations.Source{src}
}

type searchResult struct {
	Total  int `json:"total"`
	Issues []struct {
		Key    string `json:"key"`
		Fields struct {
			Summary     string    `json:"summary"`
			Description *string   `json:"description"`
			Assignee    *struct{} `json:"assignee"`
		} `json:"fields"`
	} `json:"issues"`
}

// search returns the matching tickets, assigned ones first, and how many more matched. Its errors
// never wrap the client's, and never hold the token.
func (j *Jira) search(ctx context.Context, cfg integrations.Config) ([]integrations.Ticket, int, error) {
	base := strings.TrimRight(cfg.Values["url"], "/")
	q := url.Values{"jql": {cfg.Values["jql"]}, "fields": {"summary,description,assignee"}, "maxResults": {fmt.Sprint(maxResults)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/rest/api/2/search?"+q.Encode(), nil)
	if err != nil || req.URL.Scheme == "" || req.URL.Host == "" {
		return nil, 0, errors.New("that address isn't a URL, such as https://jira.example.com")
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Values["token"])
	req.Header.Set("Accept", "application/json")
	resp, err := j.client.Do(req)
	if err != nil {
		return nil, 0, errUnreachable
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, 0, errUnreachable
	}
	if len(body) > maxBody {
		return nil, 0, errors.New("Jira's response was too large to read")
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, 0, errRejected
	case http.StatusBadRequest:
		var bad struct {
			ErrorMessages []string `json:"errorMessages"`
		}
		if json.Unmarshal(body, &bad) == nil && len(bad.ErrorMessages) > 0 {
			return nil, 0, errors.New(strings.Join(bad.ErrorMessages, " "))
		}
		return nil, 0, errors.New("Jira didn't accept the search")
	default:
		return nil, 0, fmt.Errorf("Jira answered %d", resp.StatusCode)
	}
	var res searchResult
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, 0, errors.New("couldn't read Jira's answer — is the address right?")
	}
	tickets := make([]integrations.Ticket, 0, len(res.Issues))
	assigned := map[string]bool{}
	for _, is := range res.Issues {
		t := integrations.Ticket{Key: is.Key, Title: is.Key + " " + is.Fields.Summary, URL: base + "/browse/" + is.Key}
		if is.Fields.Description != nil {
			t.Notes = *is.Fields.Description
		}
		assigned[is.Key] = is.Fields.Assignee != nil
		tickets = append(tickets, t)
	}
	sort.SliceStable(tickets, func(a, b int) bool { return assigned[tickets[a].Key] && !assigned[tickets[b].Key] })
	return tickets, max(res.Total-len(res.Issues), 0), nil
}
