package server

// The score is the time the server itself watched a machine stay up. What an
// agent reports is only used to detect that a streak must end, never to make
// one longer: time spent cheating is still real time spent.

// Reasons a streak ended, stored in history and shown to players.
const (
	ReasonNone    = ""
	ReasonFirst   = "first"   // no streak was running
	ReasonReboot  = "reboot"  // boot id changed or uptime went backwards
	ReasonGap     = "gap"     // silent for longer than the grace period
	ReasonAnomaly = "anomaly" // uptime did not advance like the server clock
)

// Anomaly tolerance: fixed part for network latency, proportional part for
// clocks that drift without NTP.
const (
	driftFixedMs   = 15_000
	driftPerCentMs = 1 // of elapsed time, in percent
)

// streak is the part of a machine's state the rules operate on. All times are
// milliseconds; timestamps come from the server clock only.
type streak struct {
	BootHash   string
	Start      int64 // 0 when no streak is running
	LastSeen   int64
	LastUptime int64
	GapCredit  int64 // server downtime not to be held against this machine
	BestMs     int64
	TotalMs    int64
}

func (s *streak) currentMs() int64 {
	if s.Start == 0 {
		return 0
	}
	return s.LastSeen - s.Start
}

// classify decides whether a beat continues the running streak.
func (s *streak) classify(bootHash string, uptimeMs, now, maxGapMs int64) string {
	if s.Start == 0 {
		return ReasonFirst
	}
	if bootHash != s.BootHash || uptimeMs < s.LastUptime {
		return ReasonReboot
	}
	elapsed := now - s.LastSeen
	if elapsed-s.GapCredit > maxGapMs {
		return ReasonGap
	}
	drift := (uptimeMs - s.LastUptime) - elapsed
	if drift < 0 {
		drift = -drift
	}
	if drift > driftFixedMs+elapsed*driftPerCentMs/100 {
		return ReasonAnomaly
	}
	return ReasonNone
}

// apply records a beat already classified as reason.
func (s *streak) apply(reason, bootHash string, uptimeMs, now int64) {
	if reason == ReasonNone {
		s.TotalMs += now - s.LastSeen
	} else {
		s.Start = now
	}
	s.BootHash = bootHash
	s.LastSeen = now
	s.LastUptime = uptimeMs
	s.GapCredit = 0
	s.BestMs = max(s.BestMs, s.currentMs())
}
