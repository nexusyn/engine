// Package ingest contém regras de negócio do ingest (chunking, slug, etc.).
//
// Filosofia: lógica pura (sem DB, sem HTTP). Funções aceitam input, retornam
// output. Workers chamam, handlers chamam — sem efeitos colaterais.
package ingest

import (
	"strings"
	"unicode/utf8"
)

// Chunk é uma fatia de texto pronta pra embedding.
// Position = ordem dentro da page original (0-indexed).
type Chunk struct {
	Position int
	Content  string
}

// ChunkConfig parametriza o chunker.
// Defaults sensatos pro embedder (contexto amplo, granularidade boa pra recall).
type ChunkConfig struct {
	// MaxChars é o tamanho máximo de cada chunk em caracteres (UTF-8 safe).
	// Default: 1500 (~500 tokens) — boa granularidade pra recall.
	MaxChars int
	// Overlap é a quantidade de chars copiados do fim do chunk anterior pro
	// início do próximo. Preserva contexto across-chunks. Default: 200.
	Overlap int
	// MinChars: chunks menores que isso são mergeados no anterior (evita
	// chunks lixo no fim com 5 chars de "sobra"). Default: 100.
	MinChars int
}

// DefaultChunkConfig retorna config padrão pro embed.
func DefaultChunkConfig() ChunkConfig {
	return ChunkConfig{MaxChars: 1500, Overlap: 200, MinChars: 100}
}

// ChunkText parte text em chunks conforme cfg.
//
// Estratégia: sliding window por chars com overlap.
// Tenta cortar em boundary de parágrafo (\n\n), depois sentença (. ! ?),
// depois palavra (espaço). Fallback: corte hard no MaxChars.
//
// Sempre retorna pelo menos 1 chunk (mesmo se text vazio — chunk vazio).
// MaxChars=0 ou negativo desabilita chunking (retorna 1 chunk com tudo).
func ChunkText(text string, cfg ChunkConfig) []Chunk {
	if cfg.MaxChars <= 0 || utf8.RuneCountInString(text) <= cfg.MaxChars {
		return []Chunk{{Position: 0, Content: text}}
	}
	if cfg.Overlap < 0 {
		cfg.Overlap = 0
	}
	if cfg.Overlap >= cfg.MaxChars {
		cfg.Overlap = cfg.MaxChars / 4
	}

	var chunks []Chunk
	runes := []rune(text)
	totalRunes := len(runes)
	start := 0

	for start < totalRunes {
		end := start + cfg.MaxChars
		if end > totalRunes {
			end = totalRunes
		}

		// Tenta achar boundary próximo do end (busca pra trás)
		if end < totalRunes {
			end = findBoundary(runes, start, end)
		}

		piece := string(runes[start:end])
		piece = strings.TrimSpace(piece)
		if piece != "" {
			chunks = append(chunks, Chunk{Position: len(chunks), Content: piece})
		}

		if end >= totalRunes {
			break
		}
		// Próximo chunk começa com overlap
		start = end - cfg.Overlap
		if start < 0 {
			start = 0
		}
	}

	// Merge último chunk se for muito pequeno
	if cfg.MinChars > 0 && len(chunks) >= 2 {
		last := chunks[len(chunks)-1]
		if utf8.RuneCountInString(last.Content) < cfg.MinChars {
			prev := chunks[len(chunks)-2]
			merged := Chunk{Position: prev.Position, Content: prev.Content + " " + last.Content}
			chunks = append(chunks[:len(chunks)-2], merged)
		}
	}

	if len(chunks) == 0 {
		return []Chunk{{Position: 0, Content: ""}}
	}
	return chunks
}

// findBoundary tenta encontrar um split point natural perto de `target`,
// olhando pra trás. Preferência: \n\n > . > ! > ? > espaço.
// Janela de busca: 30% do chunk size atrás de target.
func findBoundary(runes []rune, start, target int) int {
	maxLookback := (target - start) * 30 / 100
	if maxLookback < 10 {
		maxLookback = 10
	}
	lookbackStart := target - maxLookback
	if lookbackStart < start {
		lookbackStart = start
	}

	// 1) Parágrafo (\n\n)
	for i := target - 1; i >= lookbackStart && i >= 1; i-- {
		if runes[i] == '\n' && runes[i-1] == '\n' {
			return i + 1
		}
	}
	// 2) Sentença (. ! ?) seguido de espaço/newline
	for i := target - 1; i >= lookbackStart && i >= 1; i-- {
		if (runes[i] == '.' || runes[i] == '!' || runes[i] == '?') &&
			(i+1 >= len(runes) || runes[i+1] == ' ' || runes[i+1] == '\n') {
			return i + 1
		}
	}
	// 3) Espaço
	for i := target - 1; i >= lookbackStart; i-- {
		if runes[i] == ' ' || runes[i] == '\n' {
			return i + 1
		}
	}
	// Fallback: corte hard
	return target
}
