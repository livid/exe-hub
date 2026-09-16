package stats

import "strconv"

// fmtSscan parses a q value; strconv keeps fmt out of the hot path.
func fmtSscan(s string, f *float64) (int, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	*f = v
	return 1, nil
}
