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
			Mint: "9raUVuzeWUk53co63M4WXLWPWE4Xc6Lpn7RS9dnkpump", MinAmount: "10000000000", Recheck: "10m"}},
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
}
