package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"exehub/internal/store"
)

// The Archives page (PLAN.md, Archives): GET /archives, every past day
// of the hub as a cell in its month's calendar, the cell's colour saying
// how many posts were sent that day. The counts cost a walk over every
// post and are the same for every reader, so they are drawn once and
// kept in memory (internal/memcache) for archivesFresh; the page's own
// words are set per request, in the reader's language.

// archivesFresh is how long the counts are kept: today's cell may run
// this far behind the feed.
const archivesFresh = 5 * time.Minute

// webArchives is the page's numbers, the same in every language: the
// months newest first, how many posts they hold and on how many days,
// and the zone the days begin in.
type webArchives struct {
	Months []webArchMonth
	Posts  int
	Days   int
	Zone   string
}

// webArchMonth is one month's calendar: its weeks, Sunday first, each
// seven cells.
type webArchMonth struct {
	Year, Month int
	Posts       int
	Weeks       [][7]webArchDay
}

// webArchDay is one cell. Day is 0 for a cell outside the month, which
// only holds the column. Off is a day that is not one of the hub's past
// days — after today, or before the hub's first — and carries no count.
// Level is the colour: 0 for a day with no post, 1 to 4 by how many.
type webArchDay struct {
	Date  string // 2006-01-02
	Day   int
	Posts int
	Level int
	Off   bool
	Today bool
}

// dayZone is where the hub's days begin: the zone the stats count their
// days in (stats.timezone, by default the hub's own), so a day is the
// same day on both pages.
func (s *Server) dayZone() *time.Location {
	if st := s.Cfg.Get().Stats; st != nil && st.Location != nil {
		return st.Location
	}
	return time.Local
}

// archives is the page's numbers as of now, from memory when they were
// drawn in the last archivesFresh. The key names the day, so the first
// reader after midnight draws the new day's cell.
func (s *Server) archives(now time.Time) (*webArchives, error) {
	loc := s.dayZone()
	now = now.In(loc)
	v, err := s.mem.Fetch("archives:"+loc.String()+":"+now.Format("2006-01-02"), archivesFresh, func() (any, error) {
		days, first, err := s.St.PostDays(loc)
		if err != nil {
			return nil, err
		}
		return buildArchives(days, first, now), nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*webArchives), nil
}

// buildArchives lays days out as calendars. Every month from the hub's
// first (the month it received its first post) to this one stands whole,
// a quiet month too. An older month stands only when it has posts: a post
// pulled from a peer may be older than this hub, but a post's time is its
// author's claim, and one claim of 1970 must not draw fifty years of
// empty months. A post dated after today counts as today's.
func buildArchives(days []store.DayPosts, first string, now time.Time) *webArchives {
	loc := now.Location()
	today := now.Format("2006-01-02")
	if first == "" || first > today {
		first = today
	}
	n := map[string]int{}
	a := &webArchives{Zone: zoneName(now)}
	for _, d := range days {
		day := d.Day
		if day > today {
			day = today
		}
		n[day] += d.Posts
		a.Posts += d.Posts
	}
	a.Days = len(n)
	counts := make([]int, 0, len(n))
	months := map[string]bool{}
	for day, c := range n {
		counts = append(counts, c)
		months[day[:7]] = true
	}
	level := archLevels(counts)
	for m := monthOf(first, loc); m.Format("2006-01") <= today[:7]; m = m.AddDate(0, 1, 0) {
		months[m.Format("2006-01")] = true
	}
	keys := make([]string, 0, len(months))
	for k := range months {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	slices.Reverse(keys)
	for _, k := range keys {
		start := monthOf(k+"-01", loc)
		m := webArchMonth{Year: start.Year(), Month: int(start.Month())}
		var week [7]webArchDay
		for d := start; d.Month() == start.Month(); d = d.AddDate(0, 0, 1) {
			date := d.Format("2006-01-02")
			c := n[date]
			week[d.Weekday()] = webArchDay{Date: date, Day: d.Day(), Posts: c, Level: level(c),
				Off: date > today || (date < first && c == 0), Today: date == today}
			m.Posts += c
			if d.Weekday() == time.Saturday {
				m.Weeks = append(m.Weeks, week)
				week = [7]webArchDay{}
			}
		}
		if week != ([7]webArchDay{}) {
			m.Weeks = append(m.Weeks, week)
		}
		a.Months = append(a.Months, m)
	}
	return a
}

// monthOf is the first day of the month day (2006-01-02) is in, at noon:
// a day stepped from noon stays that day across a clock change.
func monthOf(day string, loc *time.Location) time.Time {
	t, err := time.ParseInLocation("2006-01-02", day[:7]+"-01", loc)
	if err != nil {
		return time.Time{}
	}
	return t.Add(12 * time.Hour)
}

// archLevels is the colour of a day by its count: 0 for none, then 1 to
// 4 by the quarter the count falls in among the days that have posts —
// so one loud day does not leave every other day the palest.
func archLevels(counts []int) func(int) int {
	slices.Sort(counts)
	var q [3]int
	for i := range q {
		if len(counts) > 0 {
			q[i] = counts[(len(counts)-1)*(i+1)/4]
		}
	}
	return func(c int) int {
		switch {
		case c <= 0:
			return 0
		case c > q[2]:
			return 4
		case c > q[1]:
			return 3
		case c > q[0]:
			return 2
		}
		return 1
	}
}

// zoneName names the zone the days begin in: its IANA name, or what its
// clock calls itself today when the hub runs in its host's zone.
func zoneName(now time.Time) string {
	if name := now.Location().String(); name != "Local" {
		return name
	}
	return now.Format("MST")
}

// MonthName is a month's caption in the page's language: "November
// 2026", "2026 年 11 月".
func (d *webData) MonthName(year, month int) string {
	names := strings.Split(d.L.T("archives.months"), ",")
	name := ""
	if month >= 1 && month <= len(names) {
		name = names[month-1]
	}
	return d.L.T("archives.month", "y", year, "m", name)
}

// dayName is a day in the page's language: "November 8, 2026",
// "2026 年 11 月 8 日".
func dayName(l *webLocale, t time.Time) string {
	names := strings.Split(l.T("archives.months"), ",")
	name := ""
	if m := int(t.Month()); m <= len(names) {
		name = names[m-1]
	}
	return l.T("archives.date", "y", t.Year(), "m", name, "d", t.Day())
}

// Weekdays heads a calendar's columns, Sunday first.
func (d *webData) Weekdays() []string {
	return strings.Split(d.L.T("archives.weekdays"), ",")
}

// handleArchivesPage: GET /archives.
func (s *Server) handleArchivesPage(w http.ResponseWriter, r *http.Request) {
	a, err := s.archives(time.Now())
	if err != nil {
		s.webError(w, r, http.StatusInternalServerError, "err.archives")
		return
	}
	rd := webReadingOf(r)
	s.webRender(w, r, http.StatusOK, &webData{
		Page: "archives", Title: rd.L.T("archives") + " · " + r.Host, Desc: rd.L.T("archives.desc", "host", r.Host),
		Canonical: true, Lang: rd.Lang, Q: rd.Q, Archives: a,
		Message: rd.L.T("archives.status", "posts", rd.L.T("archives.posts", "n", a.Posts),
			"days", rd.L.T("archives.days", "n", a.Days), "zone", a.Zone),
	})
}

// handleArchiveDayPage: GET /archives/{day}, the posts sent on one day
// (PLAN.md, Archives — One day's posts): what the day's cell counts,
// replies among them, the newest first and paged like the feed. The day
// is the calendar's own — it begins in dayZone, a time ahead of the
// hub's clock is not believed, and a post dated after today is today's —
// so a cell's number is the page's. A day with no posts has no page: so
// has one still to come, and anything that is not a date.
func (s *Server) handleArchiveDayPage(w http.ResponseWriter, r *http.Request) {
	loc := s.dayZone()
	day := r.PathValue("day")
	today := time.Now().In(loc).Format("2006-01-02")
	t, err := time.ParseInLocation("2006-01-02", day, loc)
	if err != nil || t.Format("2006-01-02") != day || day > today {
		s.webError(w, r, http.StatusNotFound, "err.noday")
		return
	}
	from, to := t.UnixMilli(), time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc).UnixMilli()
	if day == today {
		to = 1 << 62
	}
	count, err := s.St.DayCount(from, to)
	if err != nil {
		s.webError(w, r, http.StatusInternalServerError, "err.archives")
		return
	}
	if count == 0 {
		s.webError(w, r, http.StatusNotFound, "err.noday")
		return
	}
	pg, err := webPageOf(r.URL.Query(),
		func(before string, n int) ([]store.FeedPost, error) { return s.St.DayFeed(from, to, before, n) },
		func(after string, n int) ([]store.FeedPost, error) { return s.St.DayFeedNewer(from, to, after, n) })
	if errors.Is(err, store.ErrNotFound) {
		s.webError(w, r, http.StatusNotFound, "err.nopage")
		return
	}
	if err != nil {
		s.webError(w, r, http.StatusInternalServerError, "err.archives")
		return
	}
	rd := webReadingOf(r)
	if pg.Home {
		http.Redirect(w, r, "/archives/"+day+rd.Q, http.StatusFound)
		return
	}
	name := dayName(rd.L, t)
	s.webRender(w, r, http.StatusOK, &webData{
		Page: "day", Title: name + " · " + rd.L.T("archives") + " · " + r.Host, Desc: rd.L.T("archives.day", "date", day, "n", count),
		Canonical: true, Lang: rd.Lang, Q: rd.Q, Day: day, DayName: name, Count: count,
		Posts: webListed(s.webPosts(rd, pg.Posts)), Prev: pg.Prev, Next: pg.Next,
		Message: rd.L.T("archives.posts", "n", count) + " · " + zoneName(time.Now().In(loc)),
	})
}
