package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/job"
	"github.com/nexusyn/engine/internal/provider/embed"
)

// reindexEmbedResolver resolve o embed provider corrente (config global) — pra
// validar dims e logar o modelo do re-index. Interface fina (sem acoplar).
type reindexEmbedResolver interface {
	Embed(ctx context.Context) embed.Provider
}

// ReindexEmbeddingsHandler (admin-only): re-embedda TODOS os chunks com o modelo
// de embed corrente. Necessário ao TROCAR o modelo de embed — os vetores
// armazenados são do modelo antigo e não casam com queries no modelo novo.
//
// Fluxo: valida dims (schema é vector(1024)) → zera embeddings (reset_chunk_embeddings,
// cross-tenant) → enfileira EmbedBatchJob (re-embedda com o modelo corrente).
// Durante o re-index, o canal vetorial fica degradado (FTS continua) até drenar.
func ReindexEmbeddingsHandler(pool *pgxpool.Pool, resolver reindexEmbedResolver) http.HandlerFunc {
	insertClient, icErr := job.NewInsertOnlyClient(pool)
	if icErr != nil {
		return func(w http.ResponseWriter, _ *http.Request) {
			writeInternalError(w, "river init", icErr)
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if resolver == nil {
			writeError(w, http.StatusServiceUnavailable, "resolver de embed indisponível")
			return
		}
		emb := resolver.Embed(r.Context())
		if emb == nil {
			writeError(w, http.StatusUnprocessableEntity, "nenhum provider de embed configurado")
			return
		}
		if emb.Dim() != 1024 {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(
				"modelo de embed '%s' produz %d dims, mas o schema exige 1024 — troca incompatível sem migração de schema",
				emb.Model(), emb.Dim()))
			return
		}

		var n int64
		if err := pool.QueryRow(r.Context(), "SELECT reset_chunk_embeddings()").Scan(&n); err != nil {
			writeInternalError(w, "reset embeddings", err)
			return
		}
		if err := job.EnqueueEmbedBatch(r.Context(), insertClient, 2*time.Second); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("zerou %d embeddings mas falhou ao enfileirar re-embed: %v", n, err))
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"reset":  n,
			"model":  emb.Model(),
			"status": "re-embedding enfileirado",
		})
	}
}

// ReindexStatusHandler (admin-only): chunks ainda pendentes de embedding
// (progresso do re-index). 0 = terminou.
func ReindexStatusHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var pending int64
		if err := pool.QueryRow(r.Context(), "SELECT count_null_chunk_embeddings()").Scan(&pending); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"pending": pending})
	}
}
