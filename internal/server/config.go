package server

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is read from UB_* environment variables.
type Config struct {
	Addr        string
	DBPath      string
	PublicURL   string // used instead of the Host header, which clients control
	TrustedHops int    // reverse proxies in front of us, for X-Forwarded-For
	PowBits     int
	MaxGap      time.Duration
	MinInterval time.Duration
	MaxMachines int
	DownloadDir string
	AgentImage  string
}

func LoadConfig() (Config, error) {
	c := Config{
		Addr:        env("UB_ADDR", ":8080"),
		DBPath:      env("UB_DB", "/data/uptime-battle.db"),
		PublicURL:   strings.TrimRight(env("UB_PUBLIC_URL", "http://localhost:8080"), "/"),
		DownloadDir: env("UB_DOWNLOAD_DIR", "/srv/dl"),
		AgentImage:  env("UB_AGENT_IMAGE", "uptime-battle-agent:latest"),
		MinInterval: 30 * time.Second,
	}
	var err error
	if c.TrustedHops, err = envInt("UB_TRUSTED_HOPS", 0); err != nil {
		return c, err
	}
	if c.PowBits, err = envInt("UB_POW_BITS", 22); err != nil {
		return c, err
	}
	if c.MaxMachines, err = envInt("UB_MAX_MACHINES", 100_000); err != nil {
		return c, err
	}
	if c.MaxGap, err = time.ParseDuration(env("UB_MAX_GAP", "15m")); err != nil {
		return c, fmt.Errorf("UB_MAX_GAP: %w", err)
	}
	if c.PowBits < 0 || c.PowBits > 40 {
		return c, fmt.Errorf("UB_POW_BITS must be between 0 and 40")
	}
	if c.TrustedHops < 0 {
		return c, fmt.Errorf("UB_TRUSTED_HOPS must not be negative")
	}
	if c.MaxGap < 2*c.MinInterval {
		return c, fmt.Errorf("UB_MAX_GAP must be at least %s", 2*c.MinInterval)
	}
	// The URL is pasted into shell commands and install.sh.
	u, err := url.Parse(c.PublicURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		strings.ContainsAny(c.PublicURL, unsafeURLChars) {
		return c, fmt.Errorf("UB_PUBLIC_URL %q is not a plain http(s) URL", c.PublicURL)
	}
	return c, nil
}

// unsafeURLChars would change the meaning of the URL inside a shell command.
const unsafeURLChars = " '\"`$\\;&|<>(){}\n"

func (c Config) HTTPS() bool { return strings.HasPrefix(c.PublicURL, "https://") }

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}
