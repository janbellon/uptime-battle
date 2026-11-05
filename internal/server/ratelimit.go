package server

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// limiter is a token bucket per client. Memory is bounded: past maxClients the
// table is cleared, which at worst forgives some clients early.
type limiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	clients map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

const maxClients = 200_000

func newLimiter(perMinute, burst float64, now func() time.Time) *limiter {
	return &limiter{rate: perMinute / 60, burst: burst, clients: map[string]*bucket{}, now: now}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.clients[key]
	if !ok {
		if len(l.clients) >= maxClients {
			clear(l.clients)
		}
		b = &bucket{tokens: l.burst, last: now}
		l.clients[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops buckets that have fully refilled.
func (l *limiter) sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for k, b := range l.clients {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.clients, k)
		}
	}
}

// clientKey identifies the caller. X-Forwarded-For is client-controlled except
// for the entries our own proxies appended, so it is read from the right and
// only as far as hops says. IPv6 is grouped by /64, the usual per-subscriber
// allocation.
func clientKey(r *http.Request, hops int) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if hops > 0 {
		parts := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
		if i := len(parts) - hops; i >= 0 && strings.TrimSpace(parts[i]) != "" {
			host = strings.TrimSpace(parts[i])
		}
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	addr = addr.Unmap()
	if addr.Is6() {
		if p, err := addr.Prefix(64); err == nil {
			return p.String()
		}
	}
	return addr.String()
}
