package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"exehub/internal/store"
)

// The activity grid (PLAN.md, Activity): a year of a profile's days at
// the top of its Posts tab, and of its pictures at the top of its
// Pictures tab, as GitHub draws a year of commits — a column a week,
// Sunday at the top, the newest week at the right, a square a day in
// the Archives' five grounds. The profile's day counts are read once
// and kept in memory (internal/memcache) for archivesFresh, as the
// Archives keep theirs; the grid itself is laid out per request, in
// the reader's language.

// actWeeks is how many weeks the grid holds: this week and the 51
// before it.
const actWeeks = 52

// actDays is what is kept in memory: the store's answer.
type actDays struct {
	Days  []store.DayPosts
	First string
}

// webActivity is the grid: its weeks oldest first, the weekday letters
// at the left (every other row, as GitHub labels them), how many of
// the kind the year holds and the caption that says so, what the grid
// is for a screen reader, and the zone its days begin in.
type webActivity struct {
	Weeks   []webActWeek
	Rows    [7]string
	Total   int
	Caption string
	Label   string
	Zone    string
}

// webActWeek is one column, Sunday first, with the name of the month
// that begins in it over it ("" for none).
type webActWeek struct {
	Label string
	Days  [7]webActDay
}

// webActDay is one square. Off is a day with no ground: after today, or
// before the hub's first day. Level is its colour, 0 for none and 1 to
// 4 by the quarter its count falls in among the profile's days. Title
// is what the pointer reads over it, "" on a day that is off.
type webActDay struct {
	Date  string // 2006-01-02
	Posts int
	Level int
	Off   bool
	Today bool
	Title string
	Href  string // the day's page, for a square with a count; "" for the rest
	Sel   bool   // the day the page shows
}

// actKinds names what a grid counts: "posts" (the Posts tab) or
// "pictures" (the Pictures tab, the wall's rows). The words are
// activity.<kind>.label, .total and, for pictures, .day and .day.none;
// a day of posts is titled as the Archives title one.
var actKinds = map[string]func(*Server) func(string, *time.Location) ([]store.DayPosts, string, error){
	"posts": func(s *Server) func(string, *time.Location) ([]store.DayPosts, string, error) { return s.St.AuthorDays },
	"pictures": func(s *Server) func(string, *time.Location) ([]store.DayPosts, string, error) {
		return s.St.AuthorPictureDays
	},
}

// activity is the grid for id as of now, its counts from memory when
// they were read in the last archivesFresh. The key names the day, so
// the first reader after midnight lays the new day's square.
// base is the tab's address ("/u/{id}", "/u/{id}/pictures"), which a
// square's link takes its day after, with q the page's query; selected
// is the day the page shows, "" for the whole year.
func (s *Server) activity(id string, now time.Time, l *webLocale, kind, base, q, selected string) (*webActivity, error) {
	loc := s.dayZone()
	now = now.In(loc)
	v, err := s.mem.Fetch("activity:"+kind+":"+id+":"+loc.String()+":"+now.Format("2006-01-02"), archivesFresh, func() (any, error) {
		days, first, err := actKinds[kind](s)(id, loc)
		if err != nil {
			return nil, err
		}
		return &actDays{Days: days, First: first}, nil
	})
	if err != nil {
		return nil, err
	}
	d := v.(*actDays)
	return buildActivity(d.Days, d.First, now, l, kind, base, q, selected), nil
}

// dayWindow is the day's span in ms, as the Archives' day page has it
// (handleArchiveDayPage): the day begins in dayZone, today runs on to
// the end of time (a time ahead of the clock is not believed), and a
// day still to come, or anything that is not a date, is no day.
func (s *Server) dayWindow(day string) (from, to int64, t time.Time, ok bool) {
	loc := s.dayZone()
	today := time.Now().In(loc).Format("2006-01-02")
	t, err := time.ParseInLocation("2006-01-02", day, loc)
	if err != nil || t.Format("2006-01-02") != day || day > today {
		return 0, 0, t, false
	}
	from, to = t.UnixMilli(), time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc).UnixMilli()
	if day == today {
		to = 1 << 62
	}
	return from, to, t, true
}

// handleProfileDayPage: GET /u/{id}/{day}, the profile's posts sent on
// one day — what a square of its Posts grid counts — in the profile's
// window, the square marked, a line naming the day over the posts and
// the way back to them all. Paged like a day of the Archives. A day
// with no post of the profile's has no page.
func (s *Server) handleProfileDayPage(w http.ResponseWriter, r *http.Request) {
	id, day := r.PathValue("id"), r.PathValue("day")
	if !profileIDRe.MatchString(id) {
		s.webError(w, r, http.StatusNotFound, "err.noprofile")
		return
	}
	from, to, t, ok := s.dayWindow(day)
	if !ok {
		s.webError(w, r, http.StatusNotFound, "err.noday")
		return
	}
	count, err := s.St.AuthorDayCount(id, from, to)
	if err != nil {
		s.webError(w, r, http.StatusInternalServerError, "err.posts")
		return
	}
	if count == 0 {
		s.webError(w, r, http.StatusNotFound, "err.noday")
		return
	}
	pg, err := webPageOf(r.URL.Query(),
		func(before string, n int) ([]store.FeedPost, error) {
			return s.St.AuthorDayFeed(id, from, to, before, n)
		},
		func(after string, n int) ([]store.FeedPost, error) {
			return s.St.AuthorDayFeedNewer(id, from, to, after, n)
		})
	if errors.Is(err, store.ErrNotFound) {
		s.webError(w, r, http.StatusNotFound, "err.nopage")
		return
	}
	if err != nil {
		s.webError(w, r, http.StatusInternalServerError, "err.posts")
		return
	}
	rd := webReadingOf(r)
	if pg.Home {
		http.Redirect(w, r, "/u/"+id+"/"+day+rd.Q, http.StatusFound)
		return
	}
	pr, err := s.St.Profile(id)
	if errors.Is(err, store.ErrNotFound) {
		pr, err = &store.Profile{ID: id}, nil
	}
	if err != nil {
		s.webError(w, r, http.StatusInternalServerError, "err.profile")
		return
	}
	d := &webData{Page: "profile", Posts: webListed(s.webQuoted(rd, s.webPosts(rd, pg.Posts))), Prev: pg.Prev, Next: pg.Next,
		Lang: rd.Lang, Q: rd.Q, NavFor: id, Day: day, DayName: dayName(rd.L, t), DayCount: count}
	if err := s.profileHead(d, pr); err != nil {
		s.webError(w, r, http.StatusInternalServerError, "err.profile")
		return
	}
	if d.Activity, err = s.activity(id, time.Now(), rd.L, "posts", "/u/"+id, rd.Q, day); err != nil {
		s.webError(w, r, http.StatusInternalServerError, "err.profile")
		return
	}
	name := profileName(pr)
	d.Title = d.DayName + " · " + rd.L.T("title.on", "name", name, "host", r.Host)
	d.Desc = rd.L.T("activity.day.desc", "name", name, "date", d.DayName, "n", count)
	d.Canonical = pg.Prev == "" && pg.Next == "" || r.URL.Query().Get("before") == "" && r.URL.Query().Get("after") == ""
	s.webRender(w, r, http.StatusOK, d)
}

// buildActivity lays the days out: actWeeks columns ending with the
// week that holds today, Sunday first. A post dated after today counts
// as today's (its time is its author's claim, as the Archives weigh
// it); a day after today, or before the hub's first, is off; the levels
// are the quarters among the profile's days with posts, so a light
// poster gets the whole range too. A month's name stands over the
// column that holds its first day, except in the last column, where
// the frame would cut it: a name is about two columns wide.
func buildActivity(days []store.DayPosts, first string, now time.Time, l *webLocale, kind, base, q, selected string) *webActivity {
	dayKey, noneKey := "archives.day", "archives.day.none"
	if kind != "posts" {
		dayKey, noneKey = "activity."+kind+".day", "activity."+kind+".day.none"
	}
	loc := now.Location()
	today := now.Format("2006-01-02")
	if first == "" || first > today {
		first = today
	}
	n := map[string]int{}
	counts := []int{}
	for _, d := range days {
		day := d.Day
		if day > today {
			day = today
		}
		n[day] += d.Posts
	}
	for _, c := range n {
		counts = append(counts, c)
	}
	level := archLevels(counts)
	months := strings.Split(l.T("activity.months"), ",")
	letters := strings.Split(l.T("archives.weekdays"), ",")
	a := &webActivity{Zone: zoneName(now), Label: l.T("activity." + kind + ".label")}
	for i := 1; i < 7 && i < len(letters); i += 2 {
		a.Rows[i] = letters[i]
	}
	// the first day of the grid: the Sunday actWeeks-1 weeks before this
	// week's, stepped at noon so a clock change moves no day
	noon := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, loc)
	start := noon.AddDate(0, 0, -int(noon.Weekday())-(actWeeks-1)*7)
	d := start
	for w := 0; w < actWeeks; w++ {
		var wk webActWeek
		for i := 0; i < 7; i++ {
			date := d.Format("2006-01-02")
			c := n[date]
			wk.Days[i] = webActDay{Date: date, Posts: c, Level: level(c), Off: date > today || date < first, Today: date == today}
			if !wk.Days[i].Off {
				if c > 0 {
					wk.Days[i].Title = l.T(dayKey, "date", date, "n", c)
					wk.Days[i].Href = base + "/" + date + q
					wk.Days[i].Sel = date == selected
				} else {
					wk.Days[i].Title = l.T(noneKey, "date", date)
				}
			}
			if d.Day() == 1 && w < actWeeks-1 && int(d.Month()) <= len(months) {
				wk.Label = months[d.Month()-1]
			}
			if date >= start.Format("2006-01-02") && date <= today {
				a.Total += c
			}
			d = d.AddDate(0, 0, 1)
		}
		a.Weeks = append(a.Weeks, wk)
	}
	a.Caption = l.T("activity."+kind+".total", "n", a.Total)
	return a
}
