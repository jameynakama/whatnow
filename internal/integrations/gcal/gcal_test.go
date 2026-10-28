package gcal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/jameynakama/whatnow/internal/integrations"
)

var chicago = mustZone("America/Chicago")

func mustZone(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// feed wraps events in a calendar the way Google writes one, owned by me@example.com.
func feed(events ...string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Google Inc//Google Calendar 70.9054//EN\r\n" +
		"X-WR-CALNAME:me@example.com\r\nX-WR-TIMEZONE:America/Chicago\r\n" +
		strings.Join(events, "") + "END:VCALENDAR\r\n"
}

// vevent is one VEVENT from its property lines, with the DTSTAMP every event needs.
func vevent(lines ...string) string {
	return "BEGIN:VEVENT\r\nDTSTAMP:20261001T000000Z\r\n" + strings.Join(lines, "\r\n") + "\r\nEND:VEVENT\r\n"
}

// day returns the window for a date in Chicago.
func day(date string) (time.Time, time.Time) {
	from, err := time.ParseInLocation(time.DateOnly, date, chicago)
	if err != nil {
		panic(err)
	}
	return from, from.AddDate(0, 0, 1)
}

// summary lists events as "title key series start-end" in Chicago time, for compact diffs.
func summary(events []integrations.Event) []string {
	var out []string
	for _, e := range events {
		s := e.Title + " " + e.Key + " " + e.Series + " " + e.StartsAt.In(chicago).Format("15:04")
		if e.EndsAt != nil {
			s += "-" + e.EndsAt.In(chicago).Format("15:04")
		}
		out = append(out, s)
	}
	return out
}

func mustParse(t *testing.T, body, date string) []string {
	t.Helper()
	from, to := day(date)
	events, err := parse([]byte(body), from, to)
	if err != nil {
		t.Fatal(err)
	}
	return summary(events)
}

func TestParseKeepsTimedEventsThatStartInTheWindow(t *testing.T) {
	body := feed(
		vevent("UID:one", "SUMMARY:Dentist", "DTSTART;TZID=America/Chicago:20261005T090000", "DTEND;TZID=America/Chicago:20261005T100000"),
		vevent("UID:utc", "SUMMARY:Call", "DTSTART:20261005T190000Z", "DURATION:PT45M"),
		vevent("UID:midnight", "SUMMARY:Early", "DTSTART;TZID=America/Chicago:20261005T000000"),
		vevent("UID:allday", "SUMMARY:OOO", "DTSTART;VALUE=DATE:20261005", "DTEND;VALUE=DATE:20261006"),
		vevent("UID:tomorrow", "SUMMARY:Next", "DTSTART;TZID=America/Chicago:20261006T000000"),
		vevent("UID:yesterday", "SUMMARY:Late", "DTSTART;TZID=America/Chicago:20261004T233000", "DTEND;TZID=America/Chicago:20261005T003000"),
		vevent("UID:canceled", "SUMMARY:Off", "STATUS:CANCELLED", "DTSTART;TZID=America/Chicago:20261005T110000"),
		vevent("UID:untitled", "DTSTART;TZID=America/Chicago:20261005T120000"),
	)
	want := []string{
		"Early midnight  00:00",
		"Dentist one  09:00-10:00",
		"(no title) untitled  12:00",
		"Call utc  14:00-14:45",
	}
	if diff := cmp.Diff(want, mustParse(t, body, "2026-10-05")); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
}

func TestParseDropsWhatTheOwnerDeclined(t *testing.T) {
	attend := func(uid, status string) string {
		return vevent("UID:"+uid, "SUMMARY:"+uid, "DTSTART:20261005T150000Z",
			"ATTENDEE;CN=Someone;PARTSTAT=DECLINED:mailto:someone@example.com",
			"ATTENDEE;CUTYPE=INDIVIDUAL;ROLE=REQ-PARTICIPANT;PARTSTAT="+status+";CN=Me;X-NUM-GUESTS=0:mai\r\n lto:Me@Example.com")
	}
	body := feed(attend("declined", "DECLINED"), attend("tentative", "TENTATIVE"), attend("unanswered", "NEEDS-ACTION"),
		vevent("UID:mine", "SUMMARY:mine", "DTSTART:20261005T160000Z"))
	got := mustParse(t, body, "2026-10-05")
	want := []string{"tentative tentative  10:00", "unanswered unanswered  10:00", "mine mine  11:00"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}

	// A calendar not named after an email has no owner to match, so nothing counts as declined.
	shared := strings.Replace(body, "X-WR-CALNAME:me@example.com", "X-WR-CALNAME:Family", 1)
	if got := mustParse(t, shared, "2026-10-05"); len(got) != 4 {
		t.Errorf("shared calendar = %v, want all four", got)
	}
}

func TestParseExpandsASeriesAroundItsExceptions(t *testing.T) {
	series := vevent("UID:standup", "SUMMARY:Standup", "RRULE:FREQ=DAILY",
		"DTSTART;TZID=America/Chicago:20260901T093000", "DTEND;TZID=America/Chicago:20260901T094500",
		"EXDATE;TZID=America/Chicago:20261006T093000,20261007T093000")
	moved := vevent("UID:standup", "SUMMARY:Standup (moved)",
		"RECURRENCE-ID;TZID=America/Chicago:20261008T093000",
		"DTSTART;TZID=America/Chicago:20261009T140000", "DTEND;TZID=America/Chicago:20261009T141500")
	canceled := vevent("UID:standup", "SUMMARY:Standup", "STATUS:CANCELLED",
		"RECURRENCE-ID;TZID=America/Chicago:20261010T093000", "DTSTART;TZID=America/Chicago:20261010T093000")
	body := feed(series, moved, canceled)

	cases := map[string][]string{
		"2026-10-05": {"Standup standup|20261005T143000Z standup 09:30-09:45"},
		// Both dates on one EXDATE line are skipped.
		"2026-10-06": nil,
		"2026-10-07": nil,
		// Moved off the 8th, so it's gone from the 8th...
		"2026-10-08": nil,
		// ...and on the 9th twice: the usual occurrence, and the moved one under its original key.
		"2026-10-09": {
			"Standup standup|20261009T143000Z standup 09:30-09:45",
			"Standup (moved) standup|20261008T143000Z standup 14:00-14:15",
		},
		"2026-10-10": nil,
	}
	for date, want := range cases {
		if diff := cmp.Diff(want, mustParse(t, body, date)); diff != "" {
			t.Errorf("%s (-want +got):\n%s", date, diff)
		}
	}
}

func TestParseKeepsWallTimeAcrossDST(t *testing.T) {
	// Chicago leaves daylight time on November 1st, 2026.
	body := feed(vevent("UID:weekly", "SUMMARY:Weekly", "RRULE:FREQ=WEEKLY;BYDAY=MO",
		"DTSTART;TZID=America/Chicago:20261026T090000", "DTEND;TZID=America/Chicago:20261026T100000"))
	if diff := cmp.Diff([]string{"Weekly weekly|20261102T150000Z weekly 09:00-10:00"}, mustParse(t, body, "2026-11-02")); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
}

func TestParseFollowsMonthlyByDayRules(t *testing.T) {
	// The second Tuesday of October 2026 is the 13th.
	body := feed(vevent("UID:monthly", "SUMMARY:Review", "RRULE:FREQ=MONTHLY;BYDAY=2TU",
		"DTSTART;TZID=America/Chicago:20260908T130000"))
	if got := mustParse(t, body, "2026-10-06"); len(got) != 0 {
		t.Errorf("first Tuesday = %v", got)
	}
	if diff := cmp.Diff([]string{"Review monthly|20261013T180000Z monthly 13:00"}, mustParse(t, body, "2026-10-13")); diff != "" {
		t.Errorf("second Tuesday (-want +got):\n%s", diff)
	}
}

func TestParseSkipsOneBadEvent(t *testing.T) {
	body := feed(
		vevent("UID:bad", "SUMMARY:Bad", "DTSTART;TZID=Not/AZone:20261005T090000"),
		vevent("UID:good", "SUMMARY:Good", "DTSTART:20261005T150000Z"),
	)
	if diff := cmp.Diff([]string{"Good good  10:00"}, mustParse(t, body, "2026-10-05")); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
}

func TestParseRejectsWhatIsntACalendar(t *testing.T) {
	from, to := day("2026-10-05")
	if _, err := parse([]byte("<html>Sign in</html>"), from, to); err == nil {
		t.Error("a web page parsed as a calendar")
	}
}

func TestPullReportsEachCalendarWithoutItsURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/work.ics":
			w.Write([]byte(feed(vevent("UID:one", "SUMMARY:Standup", "DTSTART:20261005T143000Z"))))
		case "/junk.ics":
			w.Write([]byte("not a calendar"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cfg := integrations.Config{Lists: map[string][]integrations.Row{"calendars": {
		{ID: "1", Values: map[string]string{"label": "Work", "url": srv.URL + "/work.ics"}},
		{ID: "2", Values: map[string]string{"label": "Personal", "url": srv.URL + "/secret-token.ics"}},
		{ID: "3", Values: map[string]string{"label": "Junk", "url": srv.URL + "/junk.ics"}},
		{ID: "4", Values: map[string]string{"label": "Gone", "url": "http://127.0.0.1:1/secret-token.ics"}},
	}}}

	from, to := day("2026-10-05")
	sources := New(nil).Pull(context.Background(), cfg, from, to)

	if len(sources) != 4 {
		t.Fatalf("sources = %+v", sources)
	}
	if s := sources[0]; s.Label != "Work" || s.Err != nil || len(s.Events) != 1 {
		t.Errorf("work = %+v", s)
	}
	wantErrs := map[string]string{
		"Personal": "Google said 404, check the address",
		"Junk":     "couldn't read the calendar",
		"Gone":     "couldn't reach Google",
	}
	for _, s := range sources[1:] {
		if s.Err == nil || s.Err.Error() != wantErrs[s.Label] {
			t.Errorf("%s: err = %v, want %q", s.Label, s.Err, wantErrs[s.Label])
		}
		if s.Err != nil && strings.Contains(s.Err.Error(), "secret-token") {
			t.Errorf("%s: error names the URL: %v", s.Label, s.Err)
		}
	}
}

func TestPullRefusesTooBigACalendar(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, maxFeed+1))
	}))
	defer srv.Close()
	cfg := integrations.Config{Lists: map[string][]integrations.Row{"calendars": {
		{ID: "1", Values: map[string]string{"label": "Huge", "url": srv.URL}},
	}}}

	from, to := day("2026-10-05")
	sources := New(nil).Pull(context.Background(), cfg, from, to)
	if err := sources[0].Err; err == nil || err.Error() != "the calendar is too large to read" {
		t.Errorf("err = %v", err)
	}
}

func TestPullGivesUpOnASlowCalendar(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	cfg := integrations.Config{Lists: map[string][]integrations.Row{"calendars": {
		{ID: "1", Values: map[string]string{"label": "Slow", "url": srv.URL}},
	}}}
	from, to := day("2026-10-05")
	sources := New(&http.Client{Timeout: 50 * time.Millisecond}).Pull(context.Background(), cfg, from, to)
	if err := sources[0].Err; err == nil || err.Error() != "Google took too long to answer" {
		t.Errorf("err = %v", err)
	}
}
