package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Challenges are stateless: an HMAC under a server secret, so handing them out
// costs no storage however many an attacker requests. Replay protection comes
// from the per-machine last_challenge_ms check when one is consumed.

const (
	kindHeartbeat = "hb"
	kindEnroll    = "enroll"

	heartbeatTTL = 60 * time.Second
	enrollTTL    = 10 * time.Minute
)

var errBadToken = errors.New("invalid or expired challenge")

type tokenIssuer struct {
	secret []byte
	now    func() time.Time
}

func (t *tokenIssuer) mac(kind, subject, ts, nonce string) string {
	h := hmac.New(sha256.New, t.secret)
	for _, f := range []string{kind, subject, ts, nonce} {
		h.Write([]byte(f))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (t *tokenIssuer) issue(kind, subject string) string {
	ts := strconv.FormatInt(t.now().UnixMilli(), 10)
	nonce := rand.Text()
	return ts + "." + nonce + "." + t.mac(kind, subject, ts, nonce)
}

// verify returns the issue time in Unix milliseconds.
func (t *tokenIssuer) verify(kind, subject, token string, ttl time.Duration) (int64, error) {
	if len(token) > 256 {
		return 0, errBadToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0, errBadToken
	}
	if !hmac.Equal([]byte(parts[2]), []byte(t.mac(kind, subject, parts[0], parts[1]))) {
		return 0, errBadToken
	}
	issued, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, errBadToken
	}
	age := t.now().UnixMilli() - issued
	if age < 0 || age > ttl.Milliseconds() {
		return 0, errBadToken
	}
	return issued, nil
}
