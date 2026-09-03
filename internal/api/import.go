package api

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	domains "github.com/nexusyn/engine/internal/core/domain"
	"github.com/nexusyn/engine/internal/job"
	"github.com/nexusyn/engine/internal/metering"
	"github.com/nexusyn/engine/internal/tenant"
)

// importPage é uma memória do payload do POST /v1/import — mesmo shape do
// GET /v1/export (o arquivo baixado entra sem transformação). `id` do export é
// ignorado (a org destino gera os seus).
type importPage struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	Domain  string `json:"domain,omitempty"`
	Project string `json:"project,omitempty"`
	Agent   string `json:"agent,omitempty"`
	// CreatedAt preserva a cronologia original (round-trip fiel). Ausente = now().
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

// ImportRequest é o body do POST /v1/import.
type ImportRequest struct {
	Pages []importPage `json:"pages"`
}

// maxImportPages é o teto de pages por request (anti-flooding; o console fatia
// o arquivo em lotes menores que isto).
const maxImportPages = 1000

// maxImportBodyBytes — cap de body dedicado do import: um lote legítimo de
// centenas de memórias passa fácil dos 8 MiB do default. Configurável via
// NEXUS_MAX_IMPORT_BODY_BYTES; default 64 MiB.
var maxImportBodyBytes = resolveMaxImportBodyBytes()

func resolveMaxImportBodyBytes() int64 {
	const def = int64(64 * 1024 * 1024) // 64 MiB
	v := os.Getenv("NEXUS_MAX_IMPORT_BODY_BYTES")
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// ImportResponse resume o que foi enfileirado vs. pulado.
type ImportResponse struct {
	Queued         int            `json:"queued"`
	Skipped        int            `json:"skipped"`
	SkippedReasons map[string]int `json:"skipped_reasons,omitempty"`
	JobIDs         []int64        `json:"job_ids"`
	Message        string         `json:"message,omitempty"`
}

// ImportHandler — POST /v1/import: importa memórias em lote (restore de backup,
// migração entre orgs, onboarding). Aceita o payload do /v1/export como está e
// enfileira 1 IngestJob por page válida — pipeline COMPLETO (chunking, redact,
// embed, dedup Módulo A, compile), então re-importar a mesma carga não duplica.
//
// Regras: só domains de ENTRADA (memory/knowledge/guideline/skill). Derivados
// (wiki/lesson/decision/error) e desconhecidos são PULADOS — não normalizados
// pra memory como no ingest: re-importar um derivado como memory duplicaria o
// conteúdo que o compile re-destila sozinho a partir das fontes importadas.
func ImportHandler(pool *pgxpool.Pool) http.HandlerFunc {
	insertClient, err := job.NewInsertOnlyClient(pool)
	if err != nil {
		return func(w http.ResponseWriter, _ *http.Request) {
			writeInternalError(w, "river init failed", err)
		}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}

		// AUD-010: org suspensa (billing) → 403, como no ingest.
		if !requireOrgActive(w, r, pool, orgID) {
			return
		}

		var req ImportRequest
		if !decodeJSONBodyLimit(w, r, &req, maxImportBodyBytes) {
			return
		}
		if len(req.Pages) == 0 {
			writeError(w, http.StatusBadRequest, "pages é obrigatório (formato do /v1/export)")
			return
		}
		if len(req.Pages) > maxImportPages {
			writeError(w, http.StatusBadRequest,
				"máximo de "+strconv.Itoa(maxImportPages)+" pages por request — fatie o arquivo em lotes")
			return
		}

		// Separa válidas de puladas ANTES da quota (pulada não conta).
		reasons := map[string]int{}
		valid := make([]importPage, 0, len(req.Pages))
		for _, p := range req.Pages {
			if strings.TrimSpace(p.Content) == "" {
				reasons["empty_content"]++
				continue
			}
			d := strings.ToLower(strings.TrimSpace(p.Domain))
			if d == "" {
				d = "memory"
			}
			if !domains.InputDomains[d] {
				// derivados (wiki/lesson/decision/error) + desconhecidos
				reasons["derived_domain"]++
				continue
			}
			p.Domain = d
			valid = append(valid, p)
		}

		// Enforcement de storage — mesma política do ingest (gated por
		// NEXUS_ENFORCE_LIMITS, fail-open em erro de checagem), mas aqui a
		// checagem considera o LOTE inteiro (used + N > max_pages → 402).
		if metering.Enforced() && len(valid) > 0 {
			var maxPages int64
			_ = pool.QueryRow(r.Context(),
				`SELECT coalesce(max_pages, 0) FROM org_limits WHERE organization_id = $1`, orgID).Scan(&maxPages)
			if maxPages > 0 {
				var used int64
				if pool.QueryRow(r.Context(),
					`SELECT count(*) FROM pages WHERE organization_id = $1`, orgID).Scan(&used) == nil &&
					used+int64(len(valid)) > maxPages {
					writeError(w, http.StatusPaymentRequired,
						"storage limit reached for your plan — upgrade to add more memories")
					return
				}
			}
		}

		// Resolve agents uma vez por slug (find-or-create, cache por request).
		agentIDs := map[string]int64{}
		jobIDs := make([]int64, 0, len(valid))
		for _, p := range valid {
			var agentID int64
			if slug := strings.TrimSpace(p.Agent); slug != "" {
				id, ok := agentIDs[slug]
				if !ok {
					id, err = resolveAgentID(r.Context(), pool, orgID, slug)
					if err != nil {
						writeInternalError(w, "agent resolve", err)
						return
					}
					agentIDs[slug] = id
				}
				agentID = id
			}

			title := p.Title
			if title == "" {
				title = "Untitled"
			}
			result, ierr := insertClient.Insert(r.Context(), job.IngestArgs{
				OrganizationID: orgID,
				AgentID:        agentID,
				Title:          title,
				Content:        p.Content,
				Domain:         p.Domain,
				Project:        metering.SanitizeProjectSlug(p.Project),
				CreatedAt:      p.CreatedAt,
			}, &river.InsertOpts{})
			if ierr != nil {
				// Aborta com o que já foi enfileirado no resumo — o dedup do
				// pipeline torna seguro re-enviar o mesmo lote depois.
				writeError(w, http.StatusInternalServerError,
					"enqueue falhou após "+strconv.Itoa(len(jobIDs))+" pages — re-envie o lote (dedup evita duplicatas)")
				return
			}
			jobIDs = append(jobIDs, result.Job.ID)
			// Metering: 1 evento de ingest por memória importada (atribuído à IA).
			recordUsage(r.Context(), pool, orgID, agentID, "ingest", "", "", 0, nil)
		}

		skipped := len(req.Pages) - len(valid)
		writeJSON(w, http.StatusAccepted, ImportResponse{
			Queued:         len(jobIDs),
			Skipped:        skipped,
			SkippedReasons: reasons,
			JobIDs:         jobIDs,
			Message:        "import enqueued, memórias serão processadas em background",
		})
	}
}
