// Package auth implementa autenticação via API tokens (Sanctum-equivalent).
//
// Formato do plain token (transmitido ao cliente uma única vez):
//
//	{id}|{random_32_bytes_hex}
//	Ex: "42|a1b2c3d4e5f6...32hexchars"
//
// O banco guarda apenas:
//   - id (BIGSERIAL — usado pra lookup direto)
//   - token_hash (SHA-256 do random_part)
//   - abilities, expires_at, etc.
//
// Vantagens vs bcrypt:
//   - Tokens são alta entropia (256 bits via crypto/rand) → SHA-256 é suficiente
//   - SHA-256 é O(1) e ~10000× mais rápido que bcrypt → não é gargalo
//   - Lookup é O(1) via index em token_hash
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	// randomBytes = 32 → 64 hex chars → 256 bits de entropia
	randomBytes = 32
)

// ErrInvalidTokenFormat indica que a string não está em "{id}|{random_hex}".
var ErrInvalidTokenFormat = errors.New("auth: token mal-formado (esperado id|random_hex)")

// ParsedToken é a separação de "{id}|{random}" em campos.
type ParsedToken struct {
	ID         int64
	RandomHex  string
	RandomHash string // SHA-256(random_hex) em hex — corresponde a token_hash na DB
}

// GenerateToken cria um novo token plain (id é setado depois do INSERT no DB).
// Retorna o plain string (NUNCA armazenar — só transmitir ao cliente uma vez) +
// o hash que vai pra api_tokens.token_hash.
//
// Workflow no handler:
//  1. plain, hash, _ := GenerateToken()
//  2. INSERT INTO api_tokens (token_hash, ...) VALUES ($hash, ...) RETURNING id
//  3. Devolver pro cliente: fmt.Sprintf("%d|%s", id, randomPart)
func GenerateToken() (randomHex string, hashHex string, err error) {
	b := make([]byte, randomBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("auth: gerar random bytes: %w", err)
	}
	randomHex = hex.EncodeToString(b)
	hashHex = hashRandom(randomHex)
	return randomHex, hashHex, nil
}

// FormatToken combina id + random em "{id}|{random}" pra entrega ao cliente.
func FormatToken(id int64, randomHex string) string {
	return strconv.FormatInt(id, 10) + "|" + randomHex
}

// ParseToken faz parsing do "Authorization: Bearer {id}|{random}" e calcula
// o hash do random_part pra match na DB.
//
// NÃO toca DB — apenas parsing + hash.
func ParseToken(raw string) (*ParsedToken, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ErrInvalidTokenFormat
	}

	parts := strings.SplitN(raw, "|", 2)
	if len(parts) != 2 {
		return nil, ErrInvalidTokenFormat
	}

	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		return nil, ErrInvalidTokenFormat
	}

	random := parts[1]
	if len(random) != randomBytes*2 { // hex = 2× bytes
		return nil, ErrInvalidTokenFormat
	}

	return &ParsedToken{
		ID:         id,
		RandomHex:  random,
		RandomHash: hashRandom(random),
	}, nil
}

// hashRandom calcula SHA-256(randomHex) em hex lowercase.
func hashRandom(randomHex string) string {
	h := sha256.Sum256([]byte(randomHex))
	return hex.EncodeToString(h[:])
}

// CompareHashes compara dois hashes em constant-time pra prevenir timing attacks.
// Usado pra match do hash do request vs hash da DB.
func CompareHashes(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// StripBearer remove o prefixo "Bearer " de um header Authorization.
// Retorna string vazia se prefixo ausente.
func StripBearer(authHeader string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return ""
	}
	return strings.TrimSpace(authHeader[len(prefix):])
}
