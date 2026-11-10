package api

import (
	"strings"
	"time"

	"exehub/internal/store"
)

// The activity grid (PLAN.md, Activity): a year of a profile's days at
// the top of its Posts tab, as GitHub draws a year of commits — a
// column a week, Sunday at the top, the newest week at the right, a
// square a day in the Archives' five grounds. The profile's day counts
// are read once and kept in memory (internal/memcache) for
// archivesFresh, as the Archives keep theirs; the grid itself is laid
// out per request, in the reader's language.

// actWeeks is how many weeks the grid holds: this week and the 51
// before it.
const actWeeks = 52

// actDays is what is kept in memory: the store's answer.
type actDays struct {
	Days  []store.DayPosts
	First string
}

// webActivity is the grid: its weeks oldest first, the weekday letters
// at the left (every other row, as GitHub labels them), how many posts
// the year holds, and the zone its days begin in.
type webActivity struct {
	Weeks []webActWeek
	Rows  [7]string
	Total int
	Zone  string
}

// webActWeek is one column, Sunday first, with the name of the month
// that begins in it over it ("" for none).
type webActWeek struct {
	Label string
	Days  [7]webActDay
}

// webActDay is one square. Off is a day with no ground: after today, or
// before the hub's first day. Level is its colour, 0 for no post and 1
// to 4 by the quarter its count falls in among the profile's days.
type webActDay struct {
	Date  string // 2006-01-02
	Posts int
	Level int
	Off   bool
	Today bool
}

// activity is the grid for id as of now, its counts from memory when
// they were read in the last archivesFresh. The key names the day, so
// the first reader after midnight lays the new day's square.
func (s *Server) activity(id string, now time.Time, l *webLocale) (*webActivity, error) {
	loc := s.dayZone()
	now = now.In(loc)
	v, err := s.mem.Fetch("activity:"+id+":"+loc.String()+":"+now.Format("2006-01-02"), archivesFresh, func() (any, error) {
		days, first, err := s.St.AuthorDays(id, loc)
		if err != nil {
			return nil, err
		}
		return &actDays{Days: days, First: first}, nil
	})
	if err != nil {
		return nil, err
	}
	d := v.(*actDays)
	return buildActivity(d.Days, d.First, now, l), nil
}

// buildActivity lays the days out: actWeeks columns ending with the
// week that holds today, Sunday first. A post dated after today counts
// as today's (its time is its author's claim, as the Archives weigh
// it); a day after today, or before the hub's first, is off; the levels
// are the quarters among the profile's days with posts, so a light
// poster gets the whole range too. A month's name stands over the
// column that holds its first day, except in the last column, where
// the frame would cut it: a name is about two columns wide.
func buildActivity(days []store.DayPosts, first string, now time.Time, l *webLocale) *webActivity {
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
	a := &webActivity{Zone: zoneName(now)}
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
	return a
}
