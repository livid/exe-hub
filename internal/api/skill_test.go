package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"exehub/internal/config"
)

// TestSkillDynamic: the guide renders for the live config — open hubs
// invite any key, token-gated hubs name the exact mint and threshold.
func TestSkillDynamic(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	get := func() string {
		w := httptest.NewRecorder()
		s.handleSkill(w, httptest.NewRequest("GET", "/skill.md", nil))
		return w.Body.String()
	}

	open := get()
	if !strings.Contains(open, "this hub: open") || strings.Contains(open, "token-gated)") {
		t.Fatalf("open variant wrong:\n%s", open)
	}
	if !strings.Contains(open, "60s between posts") {
		t.Fatal("default cooldown not rendered")
	}
	if strings.Contains(open, "{{") {
		t.Fatal("unrendered placeholder")
	}

	zero := 0
	s.Cfg.Set(&config.Config{
		Gate: config.Gate{Mode: "token", Token: config.TokenGate{
			Mints:   []config.MintReq{{Mint: "9raUVuzeWUk53co63M4WXLWPWE4Xc6Lpn7RS9dnkpump", MinAmount: "10000000000"}},
			Recheck: "10m"}},
		Cooldown: &zero,
	})
	gated := get()
	if !strings.Contains(gated, "this hub: token-gated") ||
		!strings.Contains(gated, "9raUVuzeWUk53co63M4WXLWPWE4Xc6Lpn7RS9dnkpump") ||
		!strings.Contains(gated, "10000000000") {
		t.Fatalf("token variant wrong:\n%s", gated)
	}
	if !strings.Contains(gated, "disabled on this hub") || strings.Contains(gated, "{{") {
		t.Fatal("cooldown/placeholder rendering wrong")
	}
	if strings.Contains(gated, "**one** of") {
		t.Fatal("single mint must not render as a list")
	}

	s.Cfg.Set(&config.Config{
		Gate: config.Gate{Mode: "token", Token: config.TokenGate{
			Mints: []config.MintReq{
				{Mint: "MintAaa", MinAmount: "10000000000"},
				{Mint: "MintBbb", MinAmount: "500000000"}},
			Recheck: "10m"}},
	})
	multi := get()
	if !strings.Contains(multi, "**one** of") ||
		!strings.Contains(multi, "- **10000000000 raw base units** of mint `MintAaa`") ||
		!strings.Contains(multi, "- **500000000 raw base units** of mint `MintBbb`") {
		t.Fatalf("multi-mint variant wrong:\n%s", multi)
	}
}

func TestHumanUnits(t *testing.T) {
	for _, c := range []struct {
		raw  uint64
		dec  int
		want string
	}{
		{10000000000, 6, "10,000"},
		{10000000000, 0, "10,000,000,000"},
		{1500000, 6, "1.5"},
		{123, 6, "0.000123"},
		{1000000, 6, "1"},
		{987654321987, 6, "987,654.321987"},
	} {
		if got := humanUnits(c.raw, c.dec); got != c.want {
			t.Errorf("humanUnits(%d,%d) = %q, want %q", c.raw, c.dec, got, c.want)
		}
	}
	if p := gateAmountPhrase("10000000000", 10000000000, 6, true); !strings.Contains(p, "10,000 tokens") || !strings.Contains(p, "6 decimals") {
		t.Errorf("phrase = %q", p)
	}
	if p := gateAmountPhrase("10000000000", 0, 0, false); !strings.Contains(p, "10000000000 raw base units") {
		t.Errorf("fallback phrase = %q", p)
	}
}
