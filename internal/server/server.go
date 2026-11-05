// Package server implements the Uptime Battle web application.
package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"uptime-battle/internal/protocol"
)

const (
	maxBodyBytes     = 2048
	boardSize        = 100
	snapshotInterval = 10 * time.Second
	// onlineWindow covers the slowest documented setup, a 5 minute cron job.
	onlineWindow = 7 * time.Minute
)

// reservedNames would let a player pass for the site itself.
var reservedNames = map[string]bool{
	"admin": true, "administrator": true, "root": true, "system": true, "support": true,
	"moderator": true, "staff": true, "official": true, "uptime-battle": true, "uptimebattle": true,
}

// Snapshot is the precomputed public view. Pages and the API serve it rather
// than querying per request, so traffic spikes never reach the database.
type Snapshot struct {
	At      time.Time
	Racing  []Row
	Records []Row
	Ranks   map[string]int // by name key
	Online  int
	Totals  Totals
}

// Row is an Entry as displayed.
type Row struct {
	Rank     int
	Name     string
	StreakMs int64 // includes the time since the last beat when online
	BestMs   int64
	Online   bool
	Racing   bool
	BarPct   float64
}

type Server struct {
	cfg          Config
	store        *Store
	tokens       *tokenIssuer
	now          func() time.Time
	snapshot     atomic.Pointer[Snapshot]
	pages        map[string]*page
	web          *limiter
	api          *limiter
	enroll       *limiter
	install      []byte
	assetVersion string // busts browser caches when the CSS or JS changes
	handler      http.Handler
}

func New(cfg Config, store *Store, now func() time.Time) (*Server, error) {
	secret, err := store.Secret()
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:    cfg,
		store:  store,
		now:    now,
		tokens: &tokenIssuer{secret: secret, now: now},
		web:    newLimiter(240, 120, now),
		api:    newLimiter(120, 60, now),
		enroll: newLimiter(6.0/60, 3, now),
	}
	if err := s.loadWeb(); err != nil {
		return nil, err
	}
	if err := s.Refresh(context.Background()); err != nil {
		return nil, err
	}
	s.handler = s.routes()
	return s, nil
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	web := func(h http.HandlerFunc) http.Handler { return s.limit(s.web, h) }
	api := func(h http.HandlerFunc) http.Handler { return s.limit(s.api, h) }

	mux.Handle("GET /{$}", web(s.handleRacing))
	mux.Handle("GET /records", web(s.handleRecords))
	mux.Handle("GET /m/{name}", web(s.handleMachine))
	mux.Handle("GET /install", web(s.handleInstall))
	mux.Handle("GET /fair-play", web(s.handleFairPlay))
	mux.Handle("GET /install.sh", web(s.handleInstallScript))
	mux.Handle("GET /dl/{file}", web(s.handleDownload))
	mux.Handle("GET /static/", web(s.handleStatic()))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })

	mux.Handle("GET /api/v1/leaderboard", web(s.handleLeaderboard))
	mux.Handle("GET "+protocol.PathEnrollChallenge, api(s.handleEnrollChallenge))
	mux.Handle("POST "+protocol.PathEnroll, s.limit(s.enroll, s.handleEnroll))
	mux.Handle("POST "+protocol.PathChallenge, api(s.handleChallenge))
	mux.Handle("POST "+protocol.PathHeartbeat, api(s.handleHeartbeat))
	mux.Handle("/", web(s.handleNotFound))
	return s.secure(mux)
}

// secure sets the headers every response carries. The site loads nothing from
// third parties and uses no inline script or style, so the policy is closed.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; "+
			"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if s.cfg.HTTPS() {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) limit(l *limiter, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(clientKey(r, s.cfg.TrustedHops)) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "rate limited")
			return
		}
		next(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, protocol.ErrorResponse{Error: msg})
}

// readJSON is strict on purpose: only the agent posts here.
func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || ct != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "expected application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "body too large")
		} else {
			writeError(w, http.StatusBadRequest, "malformed request")
		}
		return false
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "malformed request")
		return false
	}
	return true
}

func (s *Server) handleEnrollChallenge(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, protocol.EnrollChallengeResponse{
		Token:      s.tokens.issue(kindEnroll, ""),
		Difficulty: s.cfg.PowBits,
	})
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req protocol.EnrollRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !protocol.ValidName(req.Name) {
		writeError(w, http.StatusBadRequest, "invalid name: 3 to 32 characters among a-z A-Z 0-9 _ . -")
		return
	}
	if reservedNames[protocol.NameKey(req.Name)] {
		writeError(w, http.StatusConflict, ErrNameTaken.Error())
		return
	}
	if len(req.PublicKey) != ed25519.PublicKeySize || len(req.Signature) != ed25519.SignatureSize {
		writeError(w, http.StatusBadRequest, "malformed key or signature")
		return
	}
	if _, err := s.tokens.verify(kindEnroll, "", req.PowToken, enrollTTL); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	pub := ed25519.PublicKey(req.PublicKey)
	if !protocol.VerifyPow(req.PowToken, pub, req.PowNonce, s.cfg.PowBits) {
		writeError(w, http.StatusForbidden, "invalid proof of work")
		return
	}
	if !ed25519.Verify(pub, protocol.EnrollMessage(req.Name, pub, req.PowToken, req.PowNonce), req.Signature) {
		writeError(w, http.StatusForbidden, ErrBadSignature.Error())
		return
	}
	id, err := s.store.Enroll(r.Context(), req.Name, pub)
	switch {
	case errors.Is(err, ErrNameTaken), errors.Is(err, ErrKeyTaken):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrFull):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case err != nil:
		slog.Error("enroll", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	default:
		slog.Info("machine enrolled", "name", req.Name)
		writeJSON(w, http.StatusCreated, protocol.EnrollResponse{MachineID: id, Name: req.Name})
	}
}

func (s *Server) handleChallenge(w http.ResponseWriter, r *http.Request) {
	var req protocol.ChallengeRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !protocol.ValidMachineID(req.MachineID) {
		writeError(w, http.StatusBadRequest, "invalid machine id")
		return
	}
	writeJSON(w, http.StatusOK, protocol.ChallengeResponse{Challenge: s.tokens.issue(kindHeartbeat, req.MachineID)})
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req protocol.HeartbeatRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !protocol.ValidMachineID(req.MachineID) || !protocol.ValidBootHash(req.BootHash) ||
		req.UptimeMs < 0 || req.UptimeMs > protocol.MaxUptimeMs ||
		len(req.Signature) != ed25519.SignatureSize {
		writeError(w, http.StatusBadRequest, "malformed heartbeat")
		return
	}
	issued, err := s.tokens.verify(kindHeartbeat, req.MachineID, req.Challenge, heartbeatTTL)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	res, err := s.store.Heartbeat(r.Context(), req, issued)
	switch {
	case errors.Is(err, ErrUnknown):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrBadSignature), errors.Is(err, ErrReplay):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrDuplicate):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrTooSoon):
		w.Header().Set("Retry-After", strconv.Itoa(int(s.cfg.MinInterval.Seconds())))
		writeError(w, http.StatusTooManyRequests, err.Error())
	case err != nil:
		slog.Error("heartbeat", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	default:
		resp := protocol.HeartbeatResponse{
			StreakSeconds: res.StreakMs / 1000,
			BestSeconds:   res.BestMs / 1000,
			Reset:         res.Reset,
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

type apiRow struct {
	Rank          int    `json:"rank"`
	Name          string `json:"name"`
	StreakSeconds int64  `json:"streak_seconds"`
	BestSeconds   int64  `json:"best_seconds"`
	Online        bool   `json:"online"`
}

func (s *Server) handleLeaderboard(w http.ResponseWriter, _ *http.Request) {
	snap := s.snapshot.Load()
	rows := make([]apiRow, 0, len(snap.Racing))
	for _, r := range snap.Racing {
		rows = append(rows, apiRow{r.Rank, r.Name, r.StreakMs / 1000, r.BestMs / 1000, r.Online})
	}
	writeJSON(w, http.StatusOK, map[string]any{"generated_at": snap.At.UTC(), "racing": rows})
}

// Refresh rebuilds the public snapshot.
func (s *Server) Refresh(ctx context.Context) error {
	racing, err := s.store.Racing(ctx)
	if err != nil {
		return err
	}
	records, err := s.store.Records(ctx, boardSize)
	if err != nil {
		return err
	}
	totals, err := s.store.Totals(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	snap := &Snapshot{At: now, Totals: totals, Ranks: make(map[string]int, len(racing))}
	for i, e := range racing {
		snap.Ranks[protocol.NameKey(e.Name)] = i + 1
		online := s.online(e, now)
		if online {
			snap.Online++
		}
		if i < boardSize {
			snap.Racing = append(snap.Racing, Row{
				Rank: i + 1, Name: e.Name, StreakMs: s.streakNow(e, now), BestMs: e.BestMs,
				Online: online, Racing: true,
			})
		}
	}
	for i, e := range records {
		_, racing := snap.Ranks[protocol.NameKey(e.Name)]
		snap.Records = append(snap.Records, Row{
			Rank: i + 1, Name: e.Name, BestMs: e.BestMs, Online: s.online(e, now), Racing: racing,
		})
	}
	scaleBars(snap.Racing, func(r Row) int64 { return r.StreakMs })
	scaleBars(snap.Records, func(r Row) int64 { return r.BestMs })
	s.snapshot.Store(snap)
	return nil
}

func scaleBars(rows []Row, value func(Row) int64) {
	if len(rows) == 0 || value(rows[0]) == 0 {
		return
	}
	top := float64(value(rows[0]))
	for i := range rows {
		rows[i].BarPct = float64(value(rows[i])) / top * 100
	}
}

func (s *Server) online(e Entry, now time.Time) bool {
	return e.Start > 0 && now.UnixMilli()-e.LastSeen-e.GapCredit < onlineWindow.Milliseconds()
}

func (s *Server) streakNow(e Entry, now time.Time) int64 {
	if !s.online(e, now) {
		return e.LastSeen - e.Start
	}
	return now.UnixMilli() - e.Start
}

// Run does the periodic work until ctx ends.
func (s *Server) Run(ctx context.Context) {
	refresh := time.NewTicker(snapshotInterval)
	alive := time.NewTicker(30 * time.Second)
	hourly := time.NewTicker(time.Hour)
	defer refresh.Stop()
	defer alive.Stop()
	defer hourly.Stop()
	for {
		var err error
		select {
		case <-ctx.Done():
			return
		case <-refresh.C:
			err = s.Refresh(ctx)
		case <-alive.C:
			err = s.store.Tick()
		case <-hourly.C:
			s.web.sweep()
			s.api.sweep()
			s.enroll.sweep()
			err = s.store.Purge()
		}
		if err != nil && ctx.Err() == nil {
			slog.Error("background task", "err", err)
		}
	}
}
