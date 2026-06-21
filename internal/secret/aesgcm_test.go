package secret

import (
	"crypto/cipher"
	"crypto/rand"
	"io"
	"strings"
	"testing"
)

func randKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestEncrypt_VersionedPrefix(t *testing.T) {
	c := newTestCipher(t)
	enc, _ := c.Encrypt("x")
	if !strings.HasPrefix(enc, "v1:") {
		t.Fatalf("ciphertext novo deveria ter prefixo v1: — veio %q", enc)
	}
}

func TestDecrypt_LegacyUnprefixed(t *testing.T) {
	c := newTestCipher(t)
	enc, _ := c.Encrypt("legacy")
	bare := strings.TrimPrefix(enc, "v1:") // simula o formato antigo (sem versão)
	got, err := c.Decrypt(bare)
	if err != nil || got != "legacy" {
		t.Fatalf("legado (sem prefixo) deveria decifrar: err=%v got=%q", err, got)
	}
}

func TestDecrypt_UnknownVersion(t *testing.T) {
	c := newTestCipher(t)
	if _, err := c.Decrypt("v99:QUJDREVG"); err == nil {
		t.Fatal("versão de chave desconhecida deveria falhar")
	}
}

func TestKeyringRotation(t *testing.T) {
	oldA, _ := newAEAD(randKey(t))
	newA, _ := newAEAD(randKey(t))
	cOld := &Cipher{primaryID: "v1", primary: oldA, keyring: map[string]cipher.AEAD{"v1": oldA}}
	enc, _ := cOld.Encrypt("rotaciona") // cifrado com v1

	// Cipher novo: primária v2 + v1 no ring (decrypt-only) → decifra o antigo e cifra v2.
	cNew := &Cipher{primaryID: "v2", primary: newA, keyring: map[string]cipher.AEAD{"v2": newA, "v1": oldA}}
	got, err := cNew.Decrypt(enc)
	if err != nil || got != "rotaciona" {
		t.Fatalf("rotação: v2 deveria decifrar v1 via ring: err=%v got=%q", err, got)
	}
	if enc2, _ := cNew.Encrypt("novo"); !strings.HasPrefix(enc2, "v2:") {
		t.Fatalf("novo write deveria ser v2: — veio %q", enc2)
	}
}

func newTestCipher(t *testing.T) *Cipher {
	t.Helper()
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatal(err)
	}
	c, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	c := newTestCipher(t)
	for _, pt := range []string{"sk-abc123", "x", "uma api key bem longa com vários caracteres ç é!"} {
		enc, err := c.Encrypt(pt)
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		if enc == pt {
			t.Fatalf("ciphertext == plaintext (não cifrou)")
		}
		got, err := c.Decrypt(enc)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if got != pt {
			t.Fatalf("round-trip: got %q want %q", got, pt)
		}
	}
}

func TestEncrypt_EmptyIsEmpty(t *testing.T) {
	c := newTestCipher(t)
	enc, _ := c.Encrypt("")
	if enc != "" {
		t.Fatalf("empty plaintext deve dar empty, deu %q", enc)
	}
	dec, _ := c.Decrypt("")
	if dec != "" {
		t.Fatalf("empty ciphertext deve dar empty, deu %q", dec)
	}
}

func TestEncrypt_NonceVariesCiphertext(t *testing.T) {
	c := newTestCipher(t)
	a, _ := c.Encrypt("same")
	b, _ := c.Encrypt("same")
	if a == b {
		t.Fatalf("dois Encrypt do mesmo texto deram igual — nonce não varia")
	}
}

func TestDecrypt_WrongKeyFails(t *testing.T) {
	c1 := newTestCipher(t)
	c2 := newTestCipher(t)
	enc, _ := c1.Encrypt("secret")
	if _, err := c2.Decrypt(enc); err == nil {
		t.Fatalf("decrypt com chave errada deveria falhar")
	}
}

func TestMask(t *testing.T) {
	cases := map[string]string{"": "", "abcd": "••••", "sk-1234567": "••••4567"}
	for in, want := range cases {
		if got := Mask(in); got != want {
			t.Fatalf("Mask(%q)=%q want %q", in, got, want)
		}
	}
}
