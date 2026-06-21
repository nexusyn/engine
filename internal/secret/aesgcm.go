// Package secret cifra/decifra segredos pequenos (API keys de provider) em
// repouso com AES-256-GCM. A chave-mestra vem do env CONFIG_ENC_KEY (base64 de
// 32 bytes).
//
// Formato de saída (AUD-024): "<keyID>:base64( nonce(12) || ciphertext )". O prefixo
// de versão da chave torna o ciphertext auto-descritivo e habilita ROTAÇÃO sem re-cifrar
// tudo de uma vez. Ciphertext LEGADO (sem prefixo, formato antigo) ainda decifra com a
// chave primária (retrocompat).
//
// ── ROTAÇÃO DE CHAVE (procedimento) ──
//  1. Gere uma chave nova:  openssl rand -base64 32
//  2. No .env do engine:
//     CONFIG_ENC_KEY=<nova>             # passa a cifrar os NOVOS
//     CONFIG_ENC_KEY_ID=v2              # id da nova (incremente)
//     CONFIG_ENC_KEY_RING=v1:<antiga>  # antiga(s) só pra DECIFRAR o que já existe (CSV id:base64)
//  3. Recreate o engine. Novos writes saem como "v2:…"; os "v1:…"/legado seguem decifrando.
//  4. (Opcional) re-encrypt pass pra migrar tudo pra v2; depois remova v1 do ring.
//
// Uso: as keys de provider gravadas em platform_model_config são cifradas aqui
// antes de ir pro Postgres e decifradas só na hora de construir o provider.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ErrNoKey indica que CONFIG_ENC_KEY não está setada/é inválida.
var ErrNoKey = errors.New("secret: CONFIG_ENC_KEY ausente ou inválida (precisa de 32 bytes base64)")

// Cipher cifra com a chave PRIMÁRIA e decifra com qualquer chave do keyring (rotação).
type Cipher struct {
	primaryID string                 // id da chave que cifra os novos
	primary   cipher.AEAD            // AEAD primária
	keyring   map[string]cipher.AEAD // id → AEAD (primária + antigas, só pra decifrar)
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secret: chave precisa de 32 bytes, veio %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// FromEnv constrói o Cipher a partir de:
//   - CONFIG_ENC_KEY      (base64 de 32 bytes) — chave primária, obrigatória.
//   - CONFIG_ENC_KEY_ID   (default "v1")       — id da primária (prefixo dos novos ciphertexts).
//   - CONFIG_ENC_KEY_RING (opcional, "id:base64,id:base64") — chaves ANTIGAS, só decrypt (rotação).
func FromEnv() (*Cipher, error) {
	raw := os.Getenv("CONFIG_ENC_KEY")
	if raw == "" {
		return nil, ErrNoKey
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(key) != 32 {
		return nil, ErrNoKey
	}
	primaryID := os.Getenv("CONFIG_ENC_KEY_ID")
	if primaryID == "" {
		primaryID = "v1"
	}
	primary, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	ring := map[string]cipher.AEAD{primaryID: primary}

	// Chaves antigas (decrypt-only) pra janela de rotação.
	if extra := strings.TrimSpace(os.Getenv("CONFIG_ENC_KEY_RING")); extra != "" {
		for _, item := range strings.Split(extra, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			id, b64, ok := strings.Cut(item, ":")
			if !ok {
				return nil, fmt.Errorf("secret: CONFIG_ENC_KEY_RING item inválido (esperado id:base64)")
			}
			k, e := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
			if e != nil || len(k) != 32 {
				return nil, fmt.Errorf("secret: chave do keyring %q inválida (32 bytes base64)", id)
			}
			a, e := newAEAD(k)
			if e != nil {
				return nil, e
			}
			ring[strings.TrimSpace(id)] = a
		}
	}
	return &Cipher{primaryID: primaryID, primary: primary, keyring: ring}, nil
}

// New constrói um Cipher de chave única (id "v1") a partir de 32 bytes.
func New(key []byte) (*Cipher, error) {
	a, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return &Cipher{primaryID: "v1", primary: a, keyring: map[string]cipher.AEAD{"v1": a}}, nil
}

// Encrypt cifra plaintext → "<primaryID>:base64(nonce||ciphertext)". String vazia → "".
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, c.primary.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := c.primary.Seal(nonce, nonce, []byte(plaintext), nil)
	return c.primaryID + ":" + base64.StdEncoding.EncodeToString(ct), nil
}

// Decrypt reverte Encrypt. Aceita o formato versionado "<id>:base64" e o LEGADO (só base64,
// sem prefixo → usa a chave primária). String vazia → "".
func (c *Cipher) Decrypt(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	aead := c.primary
	b64 := encoded
	// base64 std nunca contém ':' → um ':' indica prefixo de versão de chave.
	if id, rest, ok := strings.Cut(encoded, ":"); ok {
		a, known := c.keyring[id]
		if !known {
			return "", fmt.Errorf("secret: versão de chave desconhecida %q (faltou no CONFIG_ENC_KEY_RING?)", id)
		}
		aead, b64 = a, rest
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("secret: base64 inválido: %w", err)
	}
	ns := aead.NonceSize()
	if len(data) < ns {
		return "", errors.New("secret: ciphertext curto demais")
	}
	nonce, ct := data[:ns], data[ns:]
	pt, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("secret: falha ao decifrar (chave errada?): %w", err)
	}
	return string(pt), nil
}

// Mask devolve uma versão segura pra exibir: "••••1234" (últimos 4). Vazio → "".
func Mask(plaintext string) string {
	if plaintext == "" {
		return ""
	}
	if len(plaintext) <= 4 {
		return "••••"
	}
	return "••••" + plaintext[len(plaintext)-4:]
}
