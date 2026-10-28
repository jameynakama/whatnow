// Package gcal pulls events from Google Calendar through each calendar's secret iCal address.
package gcal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	ical "github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"

	"github.com/jameynakama/whatnow/internal/integrations"
)

// Calendar is the Google Calendar integration.
type Calendar struct {
	client *http.Client
}

// New returns the integration. A nil client means one that gives up on a calendar after 30
// seconds. Go's client asks for gzip, which Google serves, so a 20 MB feed is under 2 MB over the
// wire; setting Accept-Encoding by hand would stop it decompressing.
func New(client *http.Client) *Calendar {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Calendar{client: client}
}

func (*Calendar) ID() string   { return "gcal" }
func (*Calendar) Name() string { return "Google Calendar" }

func (*Calendar) Pulls() integrations.Pulls { return integrations.PullsEvents }

func (*Calendar) Fields() []integrations.Field {
	return []integrations.Field{{
		Key:      "calendars",
		Label:    "Calendars",
		Kind:     integrations.List,
		Required: true,
		Help:     "In Google Calendar: Settings → your calendar → Integrate calendar → Secret address in iCal format.",
		Fields: []integrations.Field{
			{Key: "label", Label: "Name", Kind: integrations.Text, Required: true},
			{Key: "url", Label: "Secret address", Kind: integrations.Secret, Required: true},
		},
	}}
}

func (c *Calendar) Pull(ctx context.Context, cfg integrations.Config, from, to time.Time) []integrations.Source {
	var sources []integrations.Source
	for _, row := range cfg.Lists["calendars"] {
		src := integrations.Source{Label: row.Values["label"]}
		body, err := c.fetch(ctx, row.Values["url"])
		if err == nil {
			src.Events, err = parse(body, from, to)
		}
		src.Err = err
		sources = append(sources, src)
	}
	return sources
}

// maxFeed bounds a feed's decompressed size. Twenty MB is a generous size; past the cap the
// address isn't serving a calendar, and a hosted server shouldn't buffer whatever it is.
const maxFeed = 32 << 20

// fetch downloads a feed. Its errors never wrap the client's, which name the secret URL.
func (c *Calendar) fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.New("that address isn't a URL")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		var timeout interface{ Timeout() bool }
		if errors.As(err, &timeout) && timeout.Timeout() {
			return nil, errors.New("Google took too long to answer")
		}
		return nil, errors.New("couldn't reach Google")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Google said %d, check the address", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeed+1))
	if err != nil {
		return nil, errors.New("the download was cut off")
	}
	if len(body) > maxFeed {
		return nil, errors.New("the calendar is too large to read")
	}
	return body, nil
}

var errUnreadable = errors.New("couldn't read the calendar")

// parse returns the timed events that start in [from, to), earliest first. It drops all-day
// events, canceled ones, and ones the calendar's owner declined.
func parse(body []byte, from, to time.Time) ([]integrations.Event, error) {
	cal, err := ical.NewDecoder(bytes.NewReader(body)).Decode()
	if err != nil {
		return nil, errUnreadable
	}
	p := parser{zones: map[string]*time.Location{}, local: from.Location(), owner: owner(cal)}
	events := cal.Events()

	// A moved or canceled occurrence is its own VEVENT, with a RECURRENCE-ID naming the start it
	// replaces. It replaces it wherever it moved to, even out of the window.
	replaced := map[string]bool{}
	for _, e := range events {
		if rid := e.Props.Get(ical.PropRecurrenceID); rid != nil {
			if t, err := p.at(rid); err == nil {
				replaced[occurrenceKey(uid(e), t)] = true
			}
		}
	}

	var out []integrations.Event
	for _, e := range events {
		start := e.Props.Get(ical.PropDateTimeStart)
		if start == nil || isDate(start) || p.skip(e) {
			continue
		}
		got, err := p.event(e, start, replaced, from, to)
		if err != nil {
			// One event Google wrote oddly shouldn't cost the whole calendar.
			continue
		}
		out = append(out, got...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartsAt.Before(out[j].StartsAt) })
	return out, nil
}

type parser struct {
	// zones caches each TZID's location: loading one reads the zone file from disk, and a busy
	// calendar names one on every event.
	zones map[string]*time.Location
	// local is where a floating time, with no zone, is read.
	local *time.Location
	// owner is the calendar owner's email, or empty when the feed doesn't say.
	owner string
}

// event returns e's occurrences that start in [from, to): one for a single event, or each
// occurrence of a recurring one that hasn't been moved or canceled.
func (p parser) event(e ical.Event, startProp *ical.Prop, replaced map[string]bool, from, to time.Time) ([]integrations.Event, error) {
	start, err := p.at(startProp)
	if err != nil {
		return nil, err
	}
	length, err := p.length(e, start)
	if err != nil {
		return nil, err
	}
	id := uid(e)
	title, _ := e.Props.Text(ical.PropSummary)
	title = strings.TrimSpace(title)
	if title == "" {
		title = "(no title)"
	}
	one := func(key, series string, at time.Time) integrations.Event {
		ev := integrations.Event{Key: key, Series: series, Title: title, StartsAt: at}
		if length > 0 {
			end := at.Add(length)
			ev.EndsAt = &end
		}
		return ev
	}

	if rid := e.Props.Get(ical.PropRecurrenceID); rid != nil {
		original, err := p.at(rid)
		if err != nil || !inWindow(start, from, to) {
			return nil, err
		}
		return []integrations.Event{one(occurrenceKey(id, original), id, start)}, nil
	}

	rule, err := e.Props.RecurrenceRule()
	if err != nil {
		return nil, err
	}
	if rule == nil {
		if !inWindow(start, from, to) {
			return nil, nil
		}
		return []integrations.Event{one(id, "", start)}, nil
	}
	rule.Dtstart = start
	r, err := rrule.NewRRule(*rule)
	if err != nil {
		return nil, err
	}
	set := rrule.Set{}
	set.RRule(r)
	for _, ex := range e.Props.Values(ical.PropExceptionDates) {
		// One EXDATE line can list several dates.
		for _, v := range strings.Split(ex.Value, ",") {
			prop := ex
			prop.Value = v
			if t, err := p.at(&prop); err == nil {
				set.ExDate(t)
			}
		}
	}
	var out []integrations.Event
	for _, at := range set.Between(from, to, true) {
		key := occurrenceKey(id, at)
		if !inWindow(at, from, to) || replaced[key] {
			continue
		}
		out = append(out, one(key, id, at))
	}
	return out, nil
}

// skip says whether e is canceled or declined by the calendar's owner.
func (p parser) skip(e ical.Event) bool {
	if status, _ := e.Props.Text(ical.PropStatus); strings.EqualFold(status, "CANCELLED") {
		return true
	}
	if p.owner == "" {
		return false
	}
	for _, a := range e.Props.Values(ical.PropAttendee) {
		if email(a.Value) == p.owner {
			return strings.EqualFold(a.Params.Get(ical.ParamParticipationStatus), "DECLINED")
		}
	}
	return false
}

// length is how long e lasts, from DTEND or DURATION, or 0 when it gives no end.
func (p parser) length(e ical.Event, start time.Time) (time.Duration, error) {
	if end := e.Props.Get(ical.PropDateTimeEnd); end != nil {
		t, err := p.at(end)
		if err != nil {
			return 0, err
		}
		return max(t.Sub(start), 0), nil
	}
	if dur := e.Props.Get(ical.PropDuration); dur != nil {
		d, err := dur.Duration()
		return max(d, 0), err
	}
	return 0, nil
}

// at reads a date-time: UTC when it ends in Z, else in its TZID's zone, else floating.
func (p parser) at(prop *ical.Prop) (time.Time, error) {
	v := prop.Value
	if strings.HasSuffix(v, "Z") {
		return time.ParseInLocation("20060102T150405Z", v, time.UTC)
	}
	loc := p.local
	if tzid := prop.Params.Get(ical.PropTimezoneID); tzid != "" {
		z, ok := p.zones[tzid]
		if !ok {
			var err error
			if z, err = time.LoadLocation(tzid); err != nil {
				return time.Time{}, err
			}
			p.zones[tzid] = z
		}
		loc = z
	}
	return time.ParseInLocation("20060102T150405", v, loc)
}

// owner is the email Google names a primary calendar after, or empty when the name isn't one.
func owner(cal *ical.Calendar) string {
	name, _ := cal.Props.Text("X-WR-CALNAME")
	if !strings.Contains(name, "@") {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(name))
}

func email(attendee string) string {
	v := strings.ToLower(strings.TrimSpace(attendee))
	return strings.TrimPrefix(v, "mailto:")
}

func isDate(prop *ical.Prop) bool {
	return prop.ValueType() == ical.ValueDate || len(prop.Value) == len("20060102")
}

func uid(e ical.Event) string {
	id, _ := e.Props.Text(ical.PropUID)
	return id
}

// occurrenceKey names one occurrence of a series by its original start, which a moved
// occurrence keeps as its RECURRENCE-ID.
func occurrenceKey(uid string, start time.Time) string {
	return uid + "|" + start.UTC().Format("20060102T150405Z")
}

func inWindow(t, from, to time.Time) bool { return !t.Before(from) && t.Before(to) }
