// Package protocol holds everything the agent and the server must agree on:
// endpoint paths, JSON payloads, signed message layouts and the enrolment
// proof of work.
package protocol

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math/bits"
	"regexp"
	"strings"
)

const (
	PathEnrollChallenge = "/api/v1/enroll/challenge"
	PathEnroll          = "/api/v1/enroll"
	PathChallenge       = "/api/v1/challenge"
	PathHeartbeat       = "/api/v1/heartbeat"

	// MaxUptimeMs bounds what an agent may declare (20 years).
	MaxUptimeMs = int64(20 * 365 * 24 * 3600 * 1000)
)

type EnrollChallengeResponse struct {
	Token      string `json:"token"`
	Difficulty int    `json:"difficulty"`
}

type EnrollRequest struct {
	Name      string `json:"name"`
	PublicKey []byte `json:"public_key"`
	PowToken  string `json:"pow_token"`
	PowNonce  uint64 `json:"pow_nonce"`
	Signature []byte `json:"signature"`
}

type EnrollResponse struct {
	MachineID string `json:"machine_id"`
	Name      string `json:"name"`
}

type ChallengeRequest struct {
	MachineID string `json:"machine_id"`
}

type ChallengeResponse struct {
	Challenge string `json:"challenge"`
}

type HeartbeatRequest struct {
	MachineID string `json:"machine_id"`
	Challenge string `json:"challenge"`
	BootHash  string `json:"boot_hash"`
	UptimeMs  int64  `json:"uptime_ms"`
	Signature []byte `json:"signature"`
}

type HeartbeatResponse struct {
	StreakSeconds int64  `json:"streak_seconds"`
	BestSeconds   int64  `json:"best_seconds"`
	Reset         string `json:"reset,omitempty"` // why a new streak started, if it did
}

type ErrorResponse struct {
	Error string `json:"error"`
}

var (
	nameRe      = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{2,31}$`)
	machineIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
	bootHashRe  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidName restricts names to ASCII: nothing to escape, no homoglyph
// impersonation, safe in URLs.
func ValidName(s string) bool { return nameRe.MatchString(s) }

func ValidMachineID(s string) bool { return machineIDRe.MatchString(s) }

func ValidBootHash(s string) bool { return bootHashRe.MatchString(s) }

// NameKey is the form used to compare names, so "Foo" and "foo" collide.
func NameKey(s string) string { return strings.ToLower(s) }

// MachineID derives the public machine identifier from its key.
func MachineID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:16])
}

// BootHash hides the raw kernel boot_id. It is deliberately not salted per
// machine: the server compares it across machines to spot one boot enrolled
// under several identities.
func BootHash(bootID string) string {
	sum := sha256.Sum256(frame("uptime-battle/boot/v1", bootID))
	return hex.EncodeToString(sum[:])
}

// frame length-prefixes every field, so no two field lists encode to the same
// bytes, and the leading domain string keeps signatures for one message type
// from being valid for another.
func frame(fields ...string) []byte {
	var out []byte
	for _, f := range fields {
		out = binary.AppendUvarint(out, uint64(len(f)))
		out = append(out, f...)
	}
	return out
}

// HeartbeatMessage is the byte string an agent signs for a heartbeat.
func HeartbeatMessage(machineID, challenge, bootHash string, uptimeMs int64) []byte {
	msg := frame("uptime-battle/heartbeat/v1", machineID, challenge, bootHash)
	return binary.BigEndian.AppendUint64(msg, uint64(uptimeMs))
}

// EnrollMessage is the byte string an agent signs to prove key possession.
func EnrollMessage(name string, pub ed25519.PublicKey, powToken string, nonce uint64) []byte {
	msg := frame("uptime-battle/enroll/v1", name, string(pub), powToken)
	return binary.BigEndian.AppendUint64(msg, nonce)
}

func powBits(prefix []byte, nonce uint64) int {
	sum := sha256.Sum256(binary.BigEndian.AppendUint64(prefix, nonce))
	n := 0
	for i := 0; i < len(sum); i += 8 {
		w := binary.BigEndian.Uint64(sum[i:])
		n += bits.LeadingZeros64(w)
		if w != 0 {
			break
		}
	}
	return n
}

func powPrefix(token string, pub ed25519.PublicKey) []byte {
	return frame("uptime-battle/pow/v1", token, string(pub))
}

// VerifyPow reports whether nonce solves the puzzle. The puzzle is bound to the
// public key, so a solved token cannot be reused for another identity.
func VerifyPow(token string, pub ed25519.PublicKey, nonce uint64, difficulty int) bool {
	return powBits(powPrefix(token, pub), nonce) >= difficulty
}

// SolvePow brute-forces the puzzle.
func SolvePow(ctx context.Context, token string, pub ed25519.PublicKey, difficulty int) (uint64, error) {
	prefix := powPrefix(token, pub)
	for nonce := uint64(0); ; nonce++ {
		if nonce&0xffff == 0 && ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if powBits(prefix, nonce) >= difficulty {
			return nonce, nil
		}
	}
}
