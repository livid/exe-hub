package store

import (
	"path/filepath"
	"testing"

	"exehub/internal/token"
)

func TestCandles(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// nothing kept: no candles, and until 0 says none ever were
	if cs, since, until, err := s.LoadCandles("P", "hour/1"); err != nil || len(cs) != 0 || since != 0 || until != 0 {
		t.Fatalf("an empty store: %v %d %d %v", cs, since, until, err)
	}
	a := []token.Candle{{T: 100, Close: 1, Volume: 5}, {T: 200, Close: 2, Volume: 6}, {T: 300, Close: 3, Volume: 7}}
	if err := s.SaveCandles("P", "hour/1", a, 100, 400, 0, false); err != nil {
		t.Fatal(err)
	}
	// another size and another pool are their own
	if err := s.SaveCandles("P", "day/1", []token.Candle{{T: 1, Close: 9}}, 0, 2, 0, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCandles("Q", "hour/1", nil, 0, 7, 0, false); err != nil {
		t.Fatal(err)
	}
	cs, since, until, err := s.LoadCandles("P", "hour/1")
	if err != nil || len(cs) != 3 || cs[0] != a[0] || cs[2] != a[2] || since != 100 || until != 400 {
		t.Fatalf("kept: %v since %d until %d (%v)", cs, since, until, err)
	}
	if cs, _, until, _ := s.LoadCandles("Q", "hour/1"); len(cs) != 0 || until != 7 {
		t.Errorf("a pool with no trades: %v until %d, want none and 7", cs, until)
	}
	// more: one kept already is written over, the old ones before 200 go
	if err := s.SaveCandles("P", "hour/1", []token.Candle{{T: 300, Close: 3.5, Volume: 8}, {T: 400, Close: 4, Volume: 9}}, 200, 500, 200, false); err != nil {
		t.Fatal(err)
	}
	cs, since, until, _ = s.LoadCandles("P", "hour/1")
	if len(cs) != 3 || cs[0].T != 200 || cs[1].Close != 3.5 || cs[2].T != 400 || since != 200 || until != 500 {
		t.Errorf("after more: %v since %d until %d", cs, since, until)
	}
	// reset: only what is given stands
	if err := s.SaveCandles("P", "hour/1", []token.Candle{{T: 900, Close: 7}}, 900, 1000, 0, true); err != nil {
		t.Fatal(err)
	}
	if cs, since, until, _ = s.LoadCandles("P", "hour/1"); len(cs) != 1 || cs[0].T != 900 || since != 900 || until != 1000 {
		t.Errorf("after a reset: %v since %d until %d", cs, since, until)
	}
	if cs, _, _, _ := s.LoadCandles("P", "day/1"); len(cs) != 1 {
		t.Errorf("the other size lost its candle: %v", cs)
	}
	// the pool a mint's chart is drawn from
	if p, err := s.ChartPool("M"); err != nil || p != "" {
		t.Errorf("no pool kept: %q %v", p, err)
	}
	if err := s.SetChartPool("M", "P"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChartPool("M", "Q"); err != nil {
		t.Fatal(err)
	}
	if p, err := s.ChartPool("M"); err != nil || p != "Q" {
		t.Errorf("the pool kept: %q %v, want the later one", p, err)
	}
}
