package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"uptime-battle/internal/protocol"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type harness struct {
	t     *testing.T
	clock *clock
	srv   *Server
	store *Store
	cfg   Config
}

func newEnv(t *testing.T) *harness {
	t.Helper()
	c := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	cfg := Config{
		DBPath: filepath.Join(t.TempDir(), "test.db"), PublicURL: "https://battle.example",
		PowBits: 4, MaxGap: 15 * time.Minute, MinInterval: 30 * time.Second, MaxMachines: 100,
		DownloadDir: t.TempDir(), AgentImage: "agent:test",
	}
	store, err := OpenStore(cfg, c.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	srv, err := New(cfg, store, c.now)
	if err != nil {
		t.Fatal(err)
	}
	// The limiters have their own tests; most scenarios would trip them.
	srv.web, srv.api, srv.enroll = newLimiter(1e6, 1e6, c.now), newLimiter(1e6, 1e6, c.now), newLimiter(1e6, 1e6, c.now)
	srv.handler = srv.routes()
	return &harness{t, c, srv, store, cfg}
}

func (e *harness) do(method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		r = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(w, req)
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return v
}

type player struct {
	id   string
	name string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	boot string
}

func (e *harness) enrollRequest(name string, pub ed25519.PublicKey, priv ed25519.PrivateKey) protocol.EnrollRequest {
	e.t.Helper()
	ch := decode[protocol.EnrollChallengeResponse](e.t, e.do("GET", protocol.PathEnrollChallenge, nil))
	nonce, err := protocol.SolvePow(context.Background(), ch.Token, pub, ch.Difficulty)
	if err != nil {
		e.t.Fatal(err)
	}
	return protocol.EnrollRequest{
		Name: name, PublicKey: pub, PowToken: ch.Token, PowNonce: nonce,
		Signature: ed25519.Sign(priv, protocol.EnrollMessage(name, pub, ch.Token, nonce)),
	}
}

func (e *harness) enroll(name string) *player {
	e.t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	w := e.do("POST", protocol.PathEnroll, e.enrollRequest(name, pub, priv))
	if w.Code != http.StatusCreated {
		e.t.Fatalf("enroll: %d %s", w.Code, w.Body)
	}
	return &player{
		id: decode[protocol.EnrollResponse](e.t, w).MachineID, name: name, pub: pub, priv: priv,
		boot: protocol.BootHash(name + "-boot"),
	}
}

func (e *harness) challenge(p *player) string {
	e.t.Helper()
	w := e.do("POST", protocol.PathChallenge, protocol.ChallengeRequest{MachineID: p.id})
	if w.Code != http.StatusOK {
		e.t.Fatalf("challenge: %d %s", w.Code, w.Body)
	}
	return decode[protocol.ChallengeResponse](e.t, w).Challenge
}

func (p *player) signed(challenge string, uptimeMs int64) protocol.HeartbeatRequest {
	return protocol.HeartbeatRequest{
		MachineID: p.id, Challenge: challenge, BootHash: p.boot, UptimeMs: uptimeMs,
		Signature: ed25519.Sign(p.priv, protocol.HeartbeatMessage(p.id, challenge, p.boot, uptimeMs)),
	}
}

func (e *harness) beat(p *player, uptimeMs int64) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do("POST", protocol.PathHeartbeat, p.signed(e.challenge(p), uptimeMs))
}

func (e *harness) mustBeat(p *player, uptimeMs int64) protocol.HeartbeatResponse {
	e.t.Helper()
	w := e.beat(p, uptimeMs)
	if w.Code != http.StatusOK {
		e.t.Fatalf("beat: %d %s", w.Code, w.Body)
	}
	return decode[protocol.HeartbeatResponse](e.t, w)
}

func wantStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d: %s", w.Code, status, w.Body)
	}
}

func TestPlayThrough(t *testing.T) {
	e := newEnv(t)
	p := e.enroll("alpha")
	up := int64(3600_000)
	e.mustBeat(p, up)
	for range 5 {
		e.clock.add(time.Minute)
		up += 60_000
		e.mustBeat(p, up)
	}
	if err := e.srv.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	board := e.do("GET", "/", nil).Body.String()
	if !strings.Contains(board, "alpha") || !strings.Contains(board, "00:05:00") {
		t.Fatalf("board does not show the streak:\n%s", board)
	}
	if page := e.do("GET", "/m/alpha", nil); page.Code != 200 || !strings.Contains(page.Body.String(), "00:05:00") {
		t.Fatalf("machine page: %d", page.Code)
	}
}

func TestForgedSignatureRejected(t *testing.T) {
	e := newEnv(t)
	victim := e.enroll("victim")
	_, attacker, _ := ed25519.GenerateKey(nil)
	ch := e.challenge(victim)
	req := victim.signed(ch, 1000)
	req.Signature = ed25519.Sign(attacker, protocol.HeartbeatMessage(victim.id, ch, victim.boot, 1000))
	wantStatus(t, e.do("POST", protocol.PathHeartbeat, req), http.StatusForbidden)
}

func TestTamperedFieldsBreakSignature(t *testing.T) {
	e := newEnv(t)
	p := e.enroll("tamper")
	req := p.signed(e.challenge(p), 1000)
	req.UptimeMs = 999_999_999
	wantStatus(t, e.do("POST", protocol.PathHeartbeat, req), http.StatusForbidden)
}

func TestReplayRejected(t *testing.T) {
	e := newEnv(t)
	p := e.enroll("replay")
	req := p.signed(e.challenge(p), 1000)
	wantStatus(t, e.do("POST", protocol.PathHeartbeat, req), http.StatusOK)
	e.clock.add(40 * time.Second) // past the rate limit, inside the token TTL
	wantStatus(t, e.do("POST", protocol.PathHeartbeat, req), http.StatusForbidden)
}

func TestStaleChallengeRejected(t *testing.T) {
	e := newEnv(t)
	p := e.enroll("stale")
	ch := e.challenge(p)
	e.clock.add(heartbeatTTL + time.Second)
	wantStatus(t, e.do("POST", protocol.PathHeartbeat, p.signed(ch, 1000)), http.StatusForbidden)
}

func TestChallengeBoundToMachine(t *testing.T) {
	e := newEnv(t)
	a, b := e.enroll("first"), e.enroll("second")
	wantStatus(t, e.do("POST", protocol.PathHeartbeat, b.signed(e.challenge(a), 1000)), http.StatusForbidden)
}

func TestEnrollTokenCannotBeHeartbeatChallenge(t *testing.T) {
	e := newEnv(t)
	p := e.enroll("kinds")
	tok := decode[protocol.EnrollChallengeResponse](t, e.do("GET", protocol.PathEnrollChallenge, nil)).Token
	wantStatus(t, e.do("POST", protocol.PathHeartbeat, p.signed(tok, 1000)), http.StatusForbidden)
}

func TestBeatsCannotBeSpedUp(t *testing.T) {
	e := newEnv(t)
	p := e.enroll("rapid")
	e.mustBeat(p, 1000)
	w := e.beat(p, 2000)
	wantStatus(t, w, http.StatusTooManyRequests)
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("agent gets no Retry-After")
	}
}

func TestOneBootOneMachine(t *testing.T) {
	e := newEnv(t)
	a, b := e.enroll("original"), e.enroll("sockpuppet")
	b.boot = a.boot
	e.mustBeat(a, 1000)
	wantStatus(t, e.beat(b, 1000), http.StatusConflict)

	// A boot is released when its streak dies, e.g. the machine was re-enrolled.
	e.clock.add(16 * time.Minute)
	e.mustBeat(b, 1000+16*60_000)
}

func TestClonedKeyResetsItself(t *testing.T) {
	e := newEnv(t)
	p := e.enroll("cloned")
	up := int64(1000)
	e.mustBeat(p, up)
	e.clock.add(time.Minute)
	up += 60_000
	e.mustBeat(p, up)

	clone := *p
	clone.boot = protocol.BootHash("other-host")
	e.clock.add(time.Minute)
	if r := e.mustBeat(&clone, up+60_000); r.Reset != ReasonReboot || r.StreakSeconds != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestEnrollValidation(t *testing.T) {
	e := newEnv(t)
	e.enroll("taken")
	pub, priv, _ := ed25519.GenerateKey(nil)

	cases := map[string]struct {
		req  func() protocol.EnrollRequest
		want int
	}{
		"xss name":      {func() protocol.EnrollRequest { return e.enrollRequest("<script>x", pub, priv) }, 400},
		"sql name":      {func() protocol.EnrollRequest { return e.enrollRequest("a';DROP TABLE machines;--", pub, priv) }, 400},
		"same name":     {func() protocol.EnrollRequest { return e.enrollRequest("taken", pub, priv) }, 409},
		"case variant":  {func() protocol.EnrollRequest { return e.enrollRequest("TaKeN", pub, priv) }, 409},
		"reserved name": {func() protocol.EnrollRequest { return e.enrollRequest("Admin", pub, priv) }, 409},
		"no pow": {func() protocol.EnrollRequest {
			r := e.enrollRequest("nopow", pub, priv)
			e.srv.cfg.PowBits = 40 // the submitted solution now falls short
			return r
		}, 403},
		"bad token": {func() protocol.EnrollRequest {
			r := e.enrollRequest("badtok", pub, priv)
			r.PowToken = "1.2.3"
			return r
		}, 403},
		"name swapped after signing": {func() protocol.EnrollRequest {
			r := e.enrollRequest("signed", pub, priv)
			r.Name = "swapped"
			return r
		}, 403},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() { e.srv.cfg.PowBits = 4 }()
			wantStatus(t, e.do("POST", protocol.PathEnroll, c.req()), c.want)
		})
	}
}

func TestExpiredPowTokenRejected(t *testing.T) {
	e := newEnv(t)
	pub, priv, _ := ed25519.GenerateKey(nil)
	req := e.enrollRequest("slowpoke", pub, priv)
	e.clock.add(enrollTTL + time.Second)
	wantStatus(t, e.do("POST", protocol.PathEnroll, req), http.StatusForbidden)
}

func TestMachineCapEnforced(t *testing.T) {
	e := newEnv(t)
	e.store.cfg.MaxMachines = 1
	e.enroll("only")
	pub, priv, _ := ed25519.GenerateKey(nil)
	wantStatus(t, e.do("POST", protocol.PathEnroll, e.enrollRequest("onemore", pub, priv)), http.StatusServiceUnavailable)
}

func TestMalformedInput(t *testing.T) {
	e := newEnv(t)
	post := func(contentType, body string) int {
		req := httptest.NewRequest("POST", protocol.PathChallenge, strings.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		w := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(w, req)
		return w.Code
	}
	id := strings.Repeat("a", 32)
	for _, c := range []struct {
		name, ct, body string
		want           int
	}{
		{"form post", "application/x-www-form-urlencoded", "a=b", 415},
		{"not json", "application/json", "{", 400},
		{"unknown field", "application/json", `{"machine_id":"` + id + `","admin":true}`, 400},
		{"trailing data", "application/json", `{"machine_id":"` + id + `"} {}`, 400},
		{"oversized", "application/json", `{"machine_id":"` + strings.Repeat("a", 4096) + `"}`, 413},
		{"bad id", "application/json", `{"machine_id":"../../etc"}`, 400},
	} {
		if got := post(c.ct, c.body); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
}

func TestUptimeBounds(t *testing.T) {
	e := newEnv(t)
	p := e.enroll("bounds")
	wantStatus(t, e.beat(p, -1), http.StatusBadRequest)
	wantStatus(t, e.beat(p, protocol.MaxUptimeMs+1), http.StatusBadRequest)
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/", "/nope", "/api/v1/leaderboard", "/static/app.css"} {
		h := e.do("GET", path, nil).Header()
		csp := h.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "unsafe") {
			t.Errorf("%s: CSP %q", path, csp)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Strict-Transport-Security") == "" {
			t.Errorf("%s: missing headers: %v", path, h)
		}
	}
}

func TestNoInlineCodeInPages(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/", "/records", "/install", "/fair-play", "/nope"} {
		body := e.do("GET", path, nil).Body.String()
		for _, bad := range []string{"<script>", " style=", " onclick=", "<style"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s contains %q, which the CSP blocks", path, bad)
			}
		}
	}
}

func TestPublicOutputHidesBootHash(t *testing.T) {
	e := newEnv(t)
	p := e.enroll("secretive")
	e.mustBeat(p, 1000)
	e.srv.Refresh(context.Background())
	for _, path := range []string{"/", "/m/secretive", "/api/v1/leaderboard"} {
		if strings.Contains(e.do("GET", path, nil).Body.String(), p.boot) {
			t.Errorf("%s leaks the boot hash", path)
		}
	}
}

func TestDownloadsAreAllowListed(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/dl/..%2f..%2fetc%2fpasswd", "/dl/secret.db", "/static/", "/static/../templates/layout.html"} {
		w := e.do("GET", path, nil)
		if w.Code == http.StatusOK {
			t.Errorf("%s served", path)
		}
	}
}

func TestInstallScriptIgnoresHostHeader(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequest("GET", "/install.sh", nil)
	req.Host = "evil.example"
	w := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), "evil.example") || !strings.Contains(w.Body.String(), "https://battle.example") {
		t.Fatal("script must use the configured URL")
	}
}

func TestRateLimiter(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	l := newLimiter(60, 2, c.now)
	if !l.allow("a") || !l.allow("a") {
		t.Fatal("burst refused")
	}
	if l.allow("a") {
		t.Fatal("over burst allowed")
	}
	if !l.allow("b") {
		t.Fatal("clients must not share a bucket")
	}
	c.add(time.Second)
	if !l.allow("a") {
		t.Fatal("did not refill")
	}
	c.add(time.Hour)
	l.sweep()
	if len(l.clients) != 0 {
		t.Fatal("sweep kept idle clients")
	}
}

func TestEnrollIsRateLimited(t *testing.T) {
	e := newEnv(t)
	e.srv.enroll = newLimiter(6.0/60, 3, e.clock.now)
	e.srv.handler = e.srv.routes()
	last := 0
	for range 5 {
		last = e.do("POST", protocol.PathEnroll, protocol.EnrollRequest{}).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("status = %d", last)
	}
}

func TestClientKey(t *testing.T) {
	mk := func(remote string, xff ...string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		for _, v := range xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		return r
	}
	for _, c := range []struct {
		name string
		r    *http.Request
		hops int
		want string
	}{
		{"direct", mk("1.2.3.4:5"), 0, "1.2.3.4"},
		{"xff ignored without proxy", mk("1.2.3.4:5", "9.9.9.9"), 0, "1.2.3.4"},
		{"proxy", mk("10.0.0.1:5", "1.2.3.4"), 1, "1.2.3.4"},
		{"spoofed left entry", mk("10.0.0.1:5", "9.9.9.9, 1.2.3.4"), 1, "1.2.3.4"},
		{"proxy sent nothing", mk("10.0.0.1:5"), 1, "10.0.0.1"},
		{"ipv6 groups by /64", mk("[2001:db8:1:2:3:4:5:6]:5"), 0, "2001:db8:1:2::/64"},
		{"mapped v4", mk("[::ffff:1.2.3.4]:5"), 0, "1.2.3.4"},
	} {
		if got := clientKey(c.r, c.hops); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDowntimeCreditSurvivesRestart(t *testing.T) {
	e := newEnv(t)
	p := e.enroll("patient")
	up := int64(1000)
	e.mustBeat(p, up)
	e.store.Tick()

	e.clock.add(3 * time.Hour) // the server is off
	if _, err := e.store.CreditDowntime(); err != nil {
		t.Fatal(err)
	}
	e.store.Tick()
	e.srv.Refresh(context.Background())
	if len(e.srv.snapshot.Load().Racing) != 1 {
		t.Fatal("machine dropped off the board during the outage")
	}
	up += 3 * 3600_000
	if r := e.mustBeat(p, up); r.Reset != "" || r.StreakSeconds != 3*3600 {
		t.Fatalf("%+v", r)
	}
}

func TestDeadMachineGetsNoCredit(t *testing.T) {
	e := newEnv(t)
	p := e.enroll("gone")
	e.mustBeat(p, 1000)
	e.clock.add(time.Hour) // silent for an hour while the server ran
	e.store.Tick()
	e.clock.add(time.Hour)
	e.store.CreditDowntime()
	e.store.Tick()
	if r := e.mustBeat(p, 1000+2*3600_000); r.Reset != ReasonGap {
		t.Fatalf("%+v", r)
	}
}

func TestHistoryIsBounded(t *testing.T) {
	e := newEnv(t)
	p := e.enroll("flapper")
	for i := range historyPerMachine + 10 {
		p.boot = protocol.BootHash(strings.Repeat("x", i+1))
		e.mustBeat(p, 1000)
		e.clock.add(time.Minute)
	}
	h, err := e.store.History(context.Background(), "flapper")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != historyPerMachine {
		t.Fatalf("history has %d rows", len(h))
	}
}

func TestPurgeKeepsScorers(t *testing.T) {
	e := newEnv(t)
	e.enroll("squatter")
	p := e.enroll("veteran")
	up := int64(0)
	e.mustBeat(p, up)
	for range 7 { // 2 hours of beats, inside the grace period
		e.clock.add(14 * time.Minute)
		up += 14 * 60_000
		e.mustBeat(p, up)
	}
	e.clock.add(31 * 24 * time.Hour)
	if err := e.store.Purge(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Machine(context.Background(), "squatter"); err != ErrUnknown {
		t.Fatalf("squatter kept: %v", err)
	}
	if _, err := e.store.Machine(context.Background(), "veteran"); err != nil {
		t.Fatalf("veteran purged: %v", err)
	}
}

func TestConcurrentBeatsCountOnce(t *testing.T) {
	e := newEnv(t)
	p := e.enroll("racer")
	e.mustBeat(p, 1000)
	e.clock.add(time.Minute)
	reqs := make([]protocol.HeartbeatRequest, 8)
	for i := range reqs {
		reqs[i] = p.signed(e.challenge(p), 61_000)
	}
	var wg sync.WaitGroup
	codes := make([]int, len(reqs))
	for i := range reqs {
		wg.Go(func() { codes[i] = e.do("POST", protocol.PathHeartbeat, reqs[i]).Code })
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == http.StatusOK {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("%d beats accepted: %v", ok, codes)
	}
}
