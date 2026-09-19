package card

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// the cases the Hub app's tableAt is run against too
// (~/tools/playwright/exe-hub-table-test.js reads this file)
func TestTableAt(t *testing.T) {
	raw, err := os.ReadFile("testdata/tables.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Text  string
		N           int
		Align, Head []string
		Rows        [][]string
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		tb, n := TableAt(strings.Split(c.Text, "\n"), 0)
		if n != c.N || (tb == nil) != (c.N == 0) {
			t.Errorf("%s: took %d lines (table %v), want %d", c.Name, n, tb != nil, c.N)
			continue
		}
		if tb == nil {
			continue
		}
		if !reflect.DeepEqual(tb.Align, c.Align) || !reflect.DeepEqual(tb.Head, c.Head) || !reflect.DeepEqual(tb.Rows, c.Rows) {
			t.Errorf("%s:\n got %q %q %q\nwant %q %q %q", c.Name, tb.Align, tb.Head, tb.Rows, c.Align, c.Head, c.Rows)
		}
	}
}

func TestUntable(t *testing.T) {
	in := "Leaders:\n\n| Fund | Return |\n| --- | ---: |\n| [MRNY](https://x.y/) | +344% |\n| | n/a |\n\nafter | words"
	want := "Leaders:\n\nFund · Return\n[MRNY](https://x.y/) · +344%\nn/a\n\nafter | words"
	if got := Untable(in); got != want {
		t.Errorf("Untable\n got %q\nwant %q", got, want)
	}
	if got := Unlink(Untable(in)); !strings.Contains(got, "MRNY · +344%") {
		t.Errorf("a table's link goes back to its words: %q", got)
	}
}
