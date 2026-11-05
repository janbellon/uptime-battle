package server

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	texttemplate "text/template"
	"time"

	"uptime-battle/internal/protocol"
)

//go:embed web/templates web/static
var webFS embed.FS

// agentFiles lists everything /dl serves; the requested name is only ever
// compared against this list, never used to build a path.
var agentFiles = map[string]bool{
	"uptime-battle-agent-linux-amd64": true,
	"uptime-battle-agent-linux-arm64": true,
	"uptime-battle-agent-linux-armv7": true,
	"SHA256SUMS":                      true,
}

type page struct{ tmpl *template.Template }

var reasonLabels = map[string]string{
	ReasonReboot:  "Reboot",
	ReasonGap:     "Silent for too long",
	ReasonAnomaly: "Inconsistent uptime",
}

func formatDuration(ms int64) string {
	sec := ms / 1000
	d, h, m, s := sec/86400, sec/3600%24, sec/60%60, sec%60
	if d > 0 {
		return fmt.Sprintf("%d d %02d:%02d:%02d", d, h, m, s)
	}
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

func formatTime(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04 UTC")
}

func (s *Server) loadWeb() error {
	funcs := template.FuncMap{
		"duration": formatDuration,
		"datetime": formatTime,
		"reason":   func(r string) string { return reasonLabels[r] },
		"sub":      func(a, b int64) int64 { return a - b },
		"pct":      func(p float64) string { return fmt.Sprintf("%.2f", max(p, 0.5)) },
	}
	sum := sha256.New()
	for _, f := range []string{"web/static/app.css", "web/static/app.js"} {
		raw, err := webFS.ReadFile(f)
		if err != nil {
			return err
		}
		sum.Write(raw)
	}
	s.assetVersion = hex.EncodeToString(sum.Sum(nil)[:6])

	s.pages = map[string]*page{}
	for _, name := range []string{"board", "machine", "install", "fairplay", "notfound"} {
		t, err := template.New("layout.html").Funcs(funcs).
			ParseFS(webFS, "web/templates/layout.html", "web/templates/"+name+".html")
		if err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
		s.pages[name] = &page{t}
	}
	script, err := texttemplate.ParseFS(webFS, "web/templates/install.sh.tmpl")
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := script.Execute(&buf, s.cfg); err != nil {
		return err
	}
	s.install = buf.Bytes()
	return nil
}

func (s *Server) render(w http.ResponseWriter, status int, name string, data map[string]any) {
	data["PublicURL"] = s.cfg.PublicURL
	data["AssetVersion"] = s.assetVersion
	var buf bytes.Buffer
	if err := s.pages[name].tmpl.Execute(&buf, data); err != nil {
		slog.Error("render", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (s *Server) board(w http.ResponseWriter, tab string) {
	snap := s.snapshot.Load()
	rows := snap.Racing
	if tab == "records" {
		rows = snap.Records
	}
	var leaderMs int64
	if len(snap.Racing) > 0 {
		leaderMs = snap.Racing[0].StreakMs
	}
	s.render(w, http.StatusOK, "board", map[string]any{
		"Title": "Leaderboard", "Nav": "board", "Tab": tab, "Rows": rows, "Snap": snap,
		"Racing": len(snap.Ranks), "LeaderMs": leaderMs,
		"LeaderOnline": len(snap.Racing) > 0 && snap.Racing[0].Online,
	})
}

func (s *Server) handleRacing(w http.ResponseWriter, _ *http.Request)  { s.board(w, "racing") }
func (s *Server) handleRecords(w http.ResponseWriter, _ *http.Request) { s.board(w, "records") }

func (s *Server) handleMachine(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !protocol.ValidName(name) {
		s.handleNotFound(w, r)
		return
	}
	e, err := s.store.Machine(r.Context(), name)
	if errors.Is(err, ErrUnknown) {
		s.handleNotFound(w, r)
		return
	}
	if err != nil {
		slog.Error("machine page", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	history, err := s.store.History(r.Context(), name)
	if err != nil {
		slog.Error("machine history", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	now := s.now()
	rank := s.snapshot.Load().Ranks[protocol.NameKey(name)]
	alive := e.Start > 0 && now.UnixMilli()-e.LastSeen-e.GapCredit <= s.cfg.MaxGap.Milliseconds()
	var streakMs int64
	if alive {
		streakMs = s.streakNow(e, now)
	}
	var longest int64 = 1
	for _, h := range history {
		longest = max(longest, h.End-h.Start)
	}
	s.render(w, http.StatusOK, "machine", map[string]any{
		"Title": e.Name, "Nav": "board", "M": e, "Rank": rank, "Alive": alive,
		"Online": s.online(e, now), "StreakMs": streakMs, "History": history, "Longest": longest,
	})
}

func (s *Server) handleInstall(w http.ResponseWriter, _ *http.Request) {
	s.render(w, http.StatusOK, "install", map[string]any{
		"Title": "Install the agent", "Nav": "install", "AgentImage": s.cfg.AgentImage,
	})
}

func (s *Server) handleFairPlay(w http.ResponseWriter, _ *http.Request) {
	s.render(w, http.StatusOK, "fairplay", map[string]any{
		"Title": "Fair play", "Nav": "fairplay", "MaxGap": int(s.cfg.MaxGap.Minutes()),
	})
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	s.render(w, http.StatusNotFound, "notfound", map[string]any{"Title": "Page not found"})
}

func (s *Server) handleInstallScript(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(s.install)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	if !agentFiles[file] {
		s.handleNotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, filepath.Join(s.cfg.DownloadDir, file))
}

func (s *Server) handleStatic() http.HandlerFunc {
	sub, err := fs.Sub(webFS, "web/static")
	if err != nil {
		panic(err) // embedded path is a compile-time constant
	}
	files := http.StripPrefix("/static/", http.FileServerFS(sub))
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") { // no directory listing
			s.handleNotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		files.ServeHTTP(w, r)
	}
}
