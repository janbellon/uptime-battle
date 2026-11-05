package protocol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"
)

func TestFrameIsUnambiguous(t *testing.T) {
	if bytes.Equal(frame("ab", "c"), frame("a", "bc")) {
		t.Fatal("different field lists encoded identically")
	}
}

func TestPowRoundTripBoundToKey(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	other, _, _ := ed25519.GenerateKey(nil)
	nonce, err := SolvePow(context.Background(), "tok", pub, 12)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPow("tok", pub, nonce, 12) {
		t.Fatal("valid solution rejected")
	}
	if VerifyPow("tok", other, nonce, 12) && VerifyPow("tok2", pub, nonce, 12) {
		t.Fatal("solution not bound to key and token")
	}
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"pc-de-jan": true, "abc": true, "ab": false, "-abc": false,
		"<script>": false, "é-pc": false, "a b c": false,
		"abcdefghijklmnopqrstuvwxyz0123456": false,
	} {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
}
