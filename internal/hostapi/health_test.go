package hostapi

import "testing"

func TestHealthChecks(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.agents.reflectErr = "reflector: no JSON object in output"
	got := map[string]healthCheck{}
	for _, c := range h.healthChecks() {
		got[c.name] = c
	}
	for name, level := range map[string]string{
		"store": "ok", "model": "error", "reflector": "error", "learning": "ok", "skills": "ok", "lua": "ok",
	} {
		if got[name].level != level {
			t.Errorf("%s = %+v, want level %s", name, got[name], level)
		}
	}
}
