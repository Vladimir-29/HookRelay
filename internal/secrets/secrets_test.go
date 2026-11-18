package secrets

import (
	"bytes"
	"testing"
)

func testKey() []byte { return bytes.Repeat([]byte{42}, 32) }

func TestSealOpenRoundTrip(t *testing.T) {
	box, err := NewBox(testKey())
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	secret := []byte("super-secret-webhook-key-1234567890")

	ct, err := box.Seal(secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(ct, secret) {
		t.Fatal("ciphertext must not contain plaintext")
	}
	got, err := box.Open(ct)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("got %q, want %q", got, secret)
	}
}

func TestSealUniqueNonce(t *testing.T) {
	box, _ := NewBox(testKey())
	secret := []byte("same-secret")
	ct1, _ := box.Seal(secret)
	ct2, _ := box.Seal(secret)
	if bytes.Equal(ct1, ct2) {
		t.Fatal("two Seal calls must produce different ciphertexts (random nonce)")
	}
}

func TestOpenWrongKey(t *testing.T) {
	box1, _ := NewBox(testKey())
	box2, _ := NewBox(bytes.Repeat([]byte{7}, 32))

	ct, _ := box1.Seal([]byte("secret"))
	if _, err := box2.Open(ct); err == nil {
		t.Fatal("Open with wrong key must fail")
	}
}

func TestOpenTampered(t *testing.T) {
	box, _ := NewBox(testKey())
	ct, _ := box.Seal([]byte("secret"))
	ct[len(ct)-1] ^= 0xff // портим tag
	if _, err := box.Open(ct); err == nil {
		t.Fatal("Open with tampered ciphertext must fail")
	}
}

func TestOpenInvalidBlob(t *testing.T) {
	box, _ := NewBox(testKey())
	for _, blob := range [][]byte{nil, {}, []byte("short")} {
		if _, err := box.Open(blob); err == nil {
			t.Fatalf("Open(%q) must fail", blob)
		}
	}
}

func TestNewBoxRejectsBadKey(t *testing.T) {
	for _, key := range [][]byte{nil, {}, make([]byte, 16), make([]byte, 33)} {
		if _, err := NewBox(key); err == nil {
			t.Fatalf("NewBox(%d bytes) must fail", len(key))
		}
	}
}

func TestGenerateUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		s, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if len(s) != 32 {
			t.Fatalf("secret len = %d, want 32", len(s))
		}
		if seen[string(s)] {
			t.Fatal("duplicate secret generated")
		}
		seen[string(s)] = true
	}
}
