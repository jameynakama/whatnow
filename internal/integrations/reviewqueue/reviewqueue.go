// Package reviewqueue offers the pull requests Review Queue ranks for review, from its local
// server.
package reviewqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jameynakama/whatnow/internal/integrations"
)

// maxResults caps a pull. A pick list longer than this means the minimum score is too low.
const maxResults = 50

// maxBody bounds the queue response. Fifty ranked pulls is generous; past the cap the address
// isn't serving Review Queue answers, and a server shouldn't buffer whatever it is.
const maxBody = 8 << 20

var (
	errUnreachable = errors.New("Can't reach Review Queue — is it running?")
	// errSlow is a timeout, which is GitHub being slow behind Review Queue, not Review Queue down.
	errSlow = errors.New("Review Queue took over a minute — GitHub may be slow, try again")
)

// ReviewQueue is the Review Queue integration.
type ReviewQueue struct {
	client *http.Client
}

// New returns the integration. A nil client means one that gives up after 60 seconds, since
// Review Queue may scan a whole organization.
func New(client *http.Client) *ReviewQueue {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &ReviewQueue{client: client}
}

func (*ReviewQueue) ID() string                { return "reviewqueue" }
func (*ReviewQueue) Name() string              { return "Review Queue" }
func (*ReviewQueue) Pulls() integrations.Pulls { return integrations.PullsTickets }

func (*ReviewQueue) Fields() []integrations.Field {
	return []integrations.Field{
		{Key: "url", Label: "Address", Kind: integrations.Text, Required: true, Help: "Where Review Queue runs, such as http://localhost:5173"},
		{Key: "min_score", Label: "Minimum score", Kind: integrations.Text, Help: "Hide PRs scoring below this, such as 100"},
	}
}

// Pull fetches the ranked queue. The window is ignored: a review isn't for a day.
func (q *ReviewQueue) Pull(ctx context.Context, cfg integrations.Config, _, _ time.Time) []integrations.Source {
	src := integrations.Source{Label: "Review Queue"}
	src.Tickets, src.More, src.Err = q.queue(ctx, cfg)
	return []integrations.Source{src}
}

type queueResult struct {
	Pulls []struct {
		ID       string   `json:"id"`
		URL      string   `json:"url"`
		Title    string   `json:"title"`
		Score    int      `json:"score"`
		Reasons  []string `json:"reasons"`
		Estimate string   `json:"estimate"`
		Rereview bool     `json:"rereview"`
	} `json:"pulls"`
}

// queue returns the pulls scoring at least the minimum, in Review Queue's order, and how many
// more there were past the cap. Its errors never wrap the client's.
func (q *ReviewQueue) queue(ctx context.Context, cfg integrations.Config) ([]integrations.Ticket, int, error) {
	// A blank minimum keeps every score, including the negative ones Review Queue gives drafts and
	// PRs that already have their reviewers.
	minScore := math.MinInt
	if s := strings.TrimSpace(cfg.Values["min_score"]); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, 0, errors.New("minimum score must be a whole number")
		}
		minScore = n
	}
	base := strings.TrimRight(cfg.Values["url"], "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/queue", nil)
	if err != nil || req.URL.Scheme == "" || req.URL.Host == "" {
		return nil, 0, errors.New("that address isn't a URL, such as http://localhost:5173")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := q.client.Do(req)
	if err != nil {
		if errTimeout(err) {
			return nil, 0, errSlow
		}
		return nil, 0, errUnreachable
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		if errTimeout(err) {
			return nil, 0, errSlow
		}
		return nil, 0, errUnreachable
	}
	if len(body) > maxBody {
		return nil, 0, errors.New("Review Queue's response was too large to read")
	}
	if resp.StatusCode != http.StatusOK {
		var bad struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &bad) == nil && bad.Error != "" {
			return nil, 0, errors.New(bad.Error)
		}
		return nil, 0, fmt.Errorf("Review Queue answered %d", resp.StatusCode)
	}
	var res queueResult
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, 0, errors.New("couldn't read Review Queue's answer — is the address right, and is Review Queue up to date?")
	}
	tickets := []integrations.Ticket{}
	more := 0
	for _, p := range res.Pulls {
		if p.Score < minScore {
			continue
		}
		if len(tickets) == maxResults {
			more++
			continue
		}
		tickets = append(tickets, integrations.Ticket{
			Key:      p.ID,
			Title:    "Review " + p.ID + ": " + p.Title,
			Notes:    notes(p.Score, p.Reasons, p.Estimate),
			URL:      p.URL,
			Rereview: p.Rereview,
		})
	}
	return tickets, more, nil
}

func errTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// notes reads as "Score 170 · Review requested from you, Small change · 5–10 min", leaving out
// a part Review Queue left empty.
func notes(score int, reasons []string, estimate string) string {
	parts := []string{fmt.Sprintf("Score %d", score)}
	for _, p := range []string{strings.Join(reasons, ", "), estimate} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " · ")
}
