package server

import "testing"

const (
	minute = int64(60_000)
	maxGap = 15 * minute
)

func beat(s *streak, boot string, uptime, now int64) string {
	r := s.classify(boot, uptime, now, maxGap)
	s.apply(r, boot, uptime, now)
	return r
}

func TestDeclaredUptimeEarnsNothing(t *testing.T) {
	s := &streak{}
	now := int64(1_000_000_000)
	if r := beat(s, "b", 400*24*60*minute, now); r != ReasonFirst {
		t.Fatalf("reason = %q", r)
	}
	if s.currentMs() != 0 {
		t.Fatalf("a claimed 400 days scored %d ms", s.currentMs())
	}
}

func TestContinuousBeatsAccumulateServerTime(t *testing.T) {
	s := &streak{}
	now, up := int64(1_000_000_000), int64(5000)
	beat(s, "b", up, now)
	for range 10 {
		now += minute
		up += minute
		if r := beat(s, "b", up, now); r != ReasonNone {
			t.Fatalf("unexpected reset %q", r)
		}
	}
	if s.currentMs() != 10*minute || s.TotalMs != 10*minute || s.BestMs != 10*minute {
		t.Fatalf("%+v", s)
	}
}

func TestStreakEndings(t *testing.T) {
	start := func() (*streak, int64, int64) {
		s := &streak{}
		now, up := int64(1_000_000_000), 10*minute
		beat(s, "b", up, now)
		beat(s, "b", up+minute, now+minute)
		return s, now + minute, up + minute
	}
	cases := []struct {
		name          string
		boot          string
		dUptime, dNow int64
		want          string
	}{
		{"new boot id", "other", minute, minute, ReasonReboot},
		{"uptime backwards", "b", -minute, minute, ReasonReboot},
		{"silence past grace", "b", 16 * minute, 16 * minute, ReasonGap},
		{"uptime fast-forwarded", "b", 60 * minute, minute, ReasonAnomaly},
		{"uptime frozen", "b", 0, 5 * minute, ReasonAnomaly},
		{"just inside grace", "b", 15 * minute, 15 * minute, ReasonNone},
		{"latency jitter tolerated", "b", minute + 3000, minute, ReasonNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, now, up := start()
			best := s.BestMs
			if r := beat(s, c.boot, up+c.dUptime, now+c.dNow); r != c.want {
				t.Fatalf("reason = %q, want %q", r, c.want)
			}
			if c.want != ReasonNone && (s.currentMs() != 0 || s.BestMs != best) {
				t.Fatalf("streak not reset cleanly: %+v", s)
			}
		})
	}
}

func TestServerDowntimeForgivesSilenceNotReboots(t *testing.T) {
	s := &streak{}
	now, up := int64(1_000_000_000), int64(0)
	beat(s, "b", up, now)

	s.GapCredit = 120 * minute
	if r := beat(s, "b", up+121*minute, now+121*minute); r != ReasonNone {
		t.Fatalf("server outage cost a streak: %q", r)
	}
	if s.GapCredit != 0 {
		t.Fatal("credit must be consumed by the first beat")
	}

	s.GapCredit = 120 * minute
	if r := beat(s, "new", 1000, now+242*minute); r != ReasonReboot {
		t.Fatalf("reboot during outage went unnoticed: %q", r)
	}
}
