package server

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestCheckTimeoutHeaderValues(t *testing.T) {
	s := &api{timeout: 30 * time.Second}
	for _, v := range []string{"", "0", "-5", "NaN", "abc", "10", "0.5", "1e9", "1e10", "1e300", "Inf", "+Inf", "0x1p70"} {
		r := httptest.NewRequest("GET", "/probe", nil)
		if v != "" {
			r.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", v)
		}
		got := s.checkTimeout(r)
		if got <= 0 || got > 30*time.Second {
			t.Errorf("header %q: timeout %s, want within (0, 30s]", v, got)
		}
	}
}
