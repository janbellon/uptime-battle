// Command uptime-battle-agent proves to an Uptime Battle server that this
// machine is still up. It only reads /proc/uptime and the kernel boot id.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"uptime-battle/internal/protocol"
)

var version = "dev"

const (
	identityFile = "identity.json"
	beatInterval = 60 * time.Second
	maxBackoff   = 5 * time.Minute
)

const usage = `Usage: uptime-battle-agent <command> [options]

Commands:
  enroll   create this machine's key and register it
  run      send heartbeats until stopped (systemd, Docker)
  once     send one heartbeat and exit (cron)
  status   print the local identity
  version  print the version

Options:
  --server URL      server address (env UB_SERVER)
  --name NAME       public machine name (env UB_NAME)
  --state-dir DIR   where the key lives (env UB_STATE_DIR)
  --quiet           print errors only
  --insecure-http   allow plain http to a non-local server

With UB_SERVER and UB_NAME set, "run" enrols on first start.
`

type options struct {
	server, name, stateDir string
	quiet, insecure        bool
}

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	var o options
	fs.StringVar(&o.server, "server", os.Getenv("UB_SERVER"), "")
	fs.StringVar(&o.name, "name", os.Getenv("UB_NAME"), "")
	fs.StringVar(&o.stateDir, "state-dir", defaultStateDir(), "")
	fs.BoolVar(&o.quiet, "quiet", false, "")
	fs.BoolVar(&o.insecure, "insecure-http", false, "")
	fs.Parse(os.Args[2:])

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "enroll":
		_, err = enroll(ctx, o)
	case "run":
		err = runLoop(ctx, o)
	case "once":
		err = once(ctx, o)
	case "status":
		err = status(o)
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("error: %v", err)
	}
}

func defaultStateDir() string {
	if d := os.Getenv("UB_STATE_DIR"); d != "" {
		return d
	}
	if os.Geteuid() == 0 {
		return "/var/lib/uptime-battle"
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "uptime-battle")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "/var/lib/uptime-battle"
	}
	return filepath.Join(home, ".local", "state", "uptime-battle")
}

// identity is the only state the agent keeps.
type identity struct {
	Server     string             `json:"server"`
	MachineID  string             `json:"machine_id"`
	Name       string             `json:"name"`
	PrivateKey ed25519.PrivateKey `json:"private_key"`
}

func loadIdentity(dir string) (*identity, error) {
	raw, err := os.ReadFile(filepath.Join(dir, identityFile))
	if err != nil {
		return nil, err
	}
	var id identity
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, fmt.Errorf("%s: %w", identityFile, err)
	}
	if len(id.PrivateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%s: bad private key", identityFile)
	}
	return &id, nil
}

func saveIdentity(dir string, id *identity) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, identityFile+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, identityFile))
}

// checkServer refuses to talk to a remote server in clear text. Signatures
// would still hold, but the machine's traffic would be visible and alterable.
func checkServer(raw string, insecure bool) (string, error) {
	if raw == "" {
		return "", errors.New("no server: use --server or UB_SERVER")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid server URL %q", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		ip := net.ParseIP(u.Hostname())
		if !insecure && u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", errors.New("refusing plain http to a remote server (--insecure-http overrides)")
		}
	default:
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	return strings.TrimRight(raw, "/"), nil
}

var client = &http.Client{
	Timeout: 20 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:        1,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

type apiError struct {
	status     int
	message    string
	retryAfter time.Duration
}

func (e *apiError) Error() string { return fmt.Sprintf("server said %d: %s", e.status, e.message) }

// call posts in (or GETs when in is nil) and decodes the JSON reply into out.
func call(ctx context.Context, base, path string, in, out any) error {
	method, body := http.MethodGet, io.Reader(nil)
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		method, body = http.MethodPost, bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "uptime-battle-agent/"+version)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e protocol.ErrorResponse
		if json.Unmarshal(raw, &e) != nil || e.Error == "" {
			e.Error = http.StatusText(resp.StatusCode)
		}
		retry, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return &apiError{resp.StatusCode, e.Error, time.Duration(retry) * time.Second}
	}
	return json.Unmarshal(raw, out)
}

func enroll(ctx context.Context, o options) (*identity, error) {
	base, err := checkServer(o.server, o.insecure)
	if err != nil {
		return nil, err
	}
	if _, err := loadIdentity(o.stateDir); err == nil {
		return nil, fmt.Errorf("already enrolled: %s exists", filepath.Join(o.stateDir, identityFile))
	}
	if !protocol.ValidName(o.name) {
		return nil, errors.New("--name must be 3 to 32 characters among a-z A-Z 0-9 _ . -")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	var ch protocol.EnrollChallengeResponse
	if err := call(ctx, base, protocol.PathEnrollChallenge, nil, &ch); err != nil {
		return nil, err
	}
	logf(o, "solving enrolment puzzle (%d bits)...", ch.Difficulty)
	nonce, err := protocol.SolvePow(ctx, ch.Token, pub, ch.Difficulty)
	if err != nil {
		return nil, err
	}
	var res protocol.EnrollResponse
	err = call(ctx, base, protocol.PathEnroll, protocol.EnrollRequest{
		Name:      o.name,
		PublicKey: pub,
		PowToken:  ch.Token,
		PowNonce:  nonce,
		Signature: ed25519.Sign(priv, protocol.EnrollMessage(o.name, pub, ch.Token, nonce)),
	}, &res)
	if err != nil {
		return nil, err
	}
	id := &identity{Server: base, MachineID: res.MachineID, Name: res.Name, PrivateKey: priv}
	if err := saveIdentity(o.stateDir, id); err != nil {
		return nil, err
	}
	logf(o, "enrolled as %s", res.Name)
	return id, nil
}

// readUptime uses /proc/uptime, which keeps counting through suspend.
func readUptime() (int64, error) {
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0, errors.New("empty /proc/uptime")
	}
	sec, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	return int64(sec * 1000), nil
}

func readBootID() (string, error) {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func beat(ctx context.Context, id *identity) (*protocol.HeartbeatResponse, error) {
	var ch protocol.ChallengeResponse
	err := call(ctx, id.Server, protocol.PathChallenge, protocol.ChallengeRequest{MachineID: id.MachineID}, &ch)
	if err != nil {
		return nil, err
	}
	uptime, err := readUptime()
	if err != nil {
		return nil, err
	}
	bootID, err := readBootID()
	if err != nil {
		return nil, err
	}
	bootHash := protocol.BootHash(bootID)
	var res protocol.HeartbeatResponse
	err = call(ctx, id.Server, protocol.PathHeartbeat, protocol.HeartbeatRequest{
		MachineID: id.MachineID,
		Challenge: ch.Challenge,
		BootHash:  bootHash,
		UptimeMs:  uptime,
		Signature: ed25519.Sign(id.PrivateKey, protocol.HeartbeatMessage(id.MachineID, ch.Challenge, bootHash, uptime)),
	}, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

func report(o options, res *protocol.HeartbeatResponse) {
	if res.Reset != "" {
		logf(o, "new streak started (%s)", res.Reset)
	}
	logf(o, "streak %s, best %s", time.Duration(res.StreakSeconds)*time.Second, time.Duration(res.BestSeconds)*time.Second)
}

func once(ctx context.Context, o options) error {
	id, err := loadIdentity(o.stateDir)
	if err != nil {
		return err
	}
	if _, err := checkServer(id.Server, o.insecure); err != nil {
		return err
	}
	res, err := beat(ctx, id)
	var api *apiError
	if errors.As(err, &api) && api.status == http.StatusTooManyRequests {
		return nil // an overlapping run already counted
	}
	if err != nil {
		return err
	}
	report(o, res)
	return nil
}

func runLoop(ctx context.Context, o options) error {
	id, err := loadIdentity(o.stateDir)
	if errors.Is(err, os.ErrNotExist) && o.server != "" && o.name != "" {
		id, err = enroll(ctx, o)
	}
	if err != nil {
		return err
	}
	if _, err := checkServer(id.Server, o.insecure); err != nil {
		return err
	}
	logf(o, "uptime-battle-agent %s: %s -> %s", version, id.Name, id.Server)

	backoff := 5 * time.Second
	for {
		wait := jitter(beatInterval)
		res, err := beat(ctx, id)
		switch {
		case err == nil:
			backoff = 5 * time.Second
			report(o, res)
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			log.Printf("heartbeat failed: %v", err)
			var api *apiError
			if errors.As(err, &api) && api.retryAfter > 0 {
				wait = api.retryAfter + jitter(5*time.Second)
			} else if !errors.As(err, &api) || api.status >= 500 {
				wait, backoff = jitter(backoff), min(backoff*2, maxBackoff)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// jitter spreads d by +-10% so a fleet restarted together does not stay in step.
func jitter(d time.Duration) time.Duration {
	span := int64(d / 5)
	n, err := rand.Int(rand.Reader, big.NewInt(span))
	if err != nil {
		return d
	}
	return d - d/10 + time.Duration(n.Int64())
}

func status(o options) error {
	id, err := loadIdentity(o.stateDir)
	if err != nil {
		return err
	}
	fmt.Printf("name:    %s\nmachine: %s\nserver:  %s\npage:    %s/m/%s\n", id.Name, id.MachineID, id.Server, id.Server, id.Name)
	return nil
}

func logf(o options, format string, args ...any) {
	if !o.quiet {
		log.Printf(format, args...)
	}
}
