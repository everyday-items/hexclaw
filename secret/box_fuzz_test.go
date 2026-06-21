package secret

// Property/fuzz coverage for the at-rest encryption Box (AES-256-GCM).
// Invariants: Open(Seal(p)) == p for ANY plaintext; Open never panics on
// arbitrary attacker-supplied input and rejects non-cipher/tampered values.

import (
	"bytes"
	"testing"
)

func testBox(t testing.TB) *Box {
	t.Helper()
	box, err := NewBox(bytes.Repeat([]byte("k"), masterKeyLen))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	return box
}

// FuzzBoxRoundTrip: ∀ plaintext, Open(Seal(p)) == p, and the sealed form always
// carries the enc:v1: prefix.
func FuzzBoxRoundTrip(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("hello"))
	f.Add([]byte("sk-secret-token-value"))
	f.Add(bytes.Repeat([]byte("x"), 4096))
	box := testBox(f)
	f.Fuzz(func(t *testing.T, plaintext []byte) {
		sealed, err := box.Seal(plaintext)
		if err != nil {
			t.Fatalf("Seal(%d bytes): %v", len(plaintext), err)
		}
		if !IsEncrypted(sealed) {
			t.Fatalf("sealed value lacks enc prefix: %q", sealed)
		}
		got, err := box.Open(sealed)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Fatalf("round-trip mismatch: got %q want %q", got, plaintext)
		}
	})
}

// FuzzBoxOpenNeverPanics: Open of ANY string must not panic — it either returns
// the plaintext (only for a genuine self-produced cipher) or an error.
func FuzzBoxOpenNeverPanics(f *testing.F) {
	f.Add("enc:v1:not-valid-base64!!!")
	f.Add("enc:v1:")
	f.Add("enc:v1:AAAA") // valid base64 but too short for a nonce
	f.Add("plaintext-no-prefix")
	f.Add("")
	box := testBox(f)
	f.Fuzz(func(t *testing.T, s string) {
		// The only property under test is "does not panic". A nil-safe error
		// return is the contract for non-cipher / tampered input.
		_, _ = box.Open(s)
	})
}

// FuzzBoxOpenWrongKeyNeverYieldsPlaintext: a cipher sealed under key A must never
// open (without error) under key B — GCM authentication must reject it.
func FuzzBoxOpenWrongKeyNeverYieldsPlaintext(f *testing.F) {
	f.Add([]byte("secret"))
	f.Add([]byte(""))
	boxA := testBox(f)
	boxB, err := NewBox(bytes.Repeat([]byte("Z"), masterKeyLen))
	if err != nil {
		f.Fatalf("NewBox B: %v", err)
	}
	f.Fuzz(func(t *testing.T, plaintext []byte) {
		sealed, err := boxA.Seal(plaintext)
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		if _, err := boxB.Open(sealed); err == nil {
			t.Fatalf("cipher opened under wrong key without error (plaintext=%q)", plaintext)
		}
	})
}
