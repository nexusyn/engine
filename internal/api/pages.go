package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/tenant"
)

type memoryItem struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Domain    string    `json:"domain"`
	Agent     string    `json:"agent"` // slug da IA que salvou ("" = sem agent)
	Preview   string    `json:"preview"`
	CreatedAt time.Time `json:"created_at"`
}

// pageFilter monta o WHERE + args do browse/export de pages de uma org, com
// filtros opcionais de domain e q (ILIKE em título OU conteúdo). $1 é sempre a
// org. `alias` prefixa as colunas ("p" quando há JOIN com agents; "" sem join).
// Compartilhado por MemoriesListHandler, MemoryIDsHandler e ExportHandler pra
// que a tabela, o "select all" e o export filtrem exatamente igual.
func pageFilter(orgID int64, domain, q, alias string) (string, []any) {
	p := ""
	if alias != "" {
		p = alias + "."
	}
	where := fmt.Sprintf("%sorganization_id = $1 AND %svalid_to IS NULL", p, p)
	args := []any{orgID}
	if domain != "" {
		args = append(args, domain)
		where += fmt.Sprintf(" AND %sdomain = $%d", p, len(args))
	}
	if q != "" {
		args = append(args, "%"+q+"%")
		where += fmt.Sprintf(" AND (%stitle ILIKE $%d OR %scontent ILIKE $%d)", p, len(args), p, len(args))
	}
	return where, args
}

// MemoriesListHandler — GET /v1/memories?domain=X&limit=N&q=texto: lista pages
// (browse) com preview do conteúdo. Backing das telas Wiki (domain=wiki) e
// Knowledge Base (domain=knowledge), além de um browse geral. `q` filtra por
// substring (ILIKE) em título OU conteúdo — é o campo de busca das tabelas do
// dashboard (filtro de browse, não ranqueado). Pra busca semântica/ranqueada
// use POST /v1/search; pra adicionar use POST /v1/ingest (com domain).
func MemoriesListHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		domain := r.URL.Query().Get("domain")
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		pg := parsePageParams(r, 50, 200)

		// Mesmo WHERE no count e no select → paginação correta (alias "p" por causa
		// do JOIN com agents).
		where, args := pageFilter(orgID, domain, q, "p")

		out := []memoryItem{}
		total := 0
		err = tenant.RunWithTenantReadOnly(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			if e := tx.QueryRow(r.Context(),
				"SELECT count(*) FROM pages p WHERE "+where, args...).Scan(&total); e != nil {
				return e
			}
			listArgs := append(append([]any{}, args...), pg.Limit, pg.Offset)
			rows, qerr := tx.Query(r.Context(), fmt.Sprintf(
				`SELECT p.id, p.title, p.domain, COALESCE(a.slug, ''), left(p.content, 400), p.created_at
				 FROM pages p LEFT JOIN agents a ON a.id = p.agent_id
				 WHERE %s
				 ORDER BY p.created_at DESC LIMIT $%d OFFSET $%d`,
				where, len(args)+1, len(args)+2), listArgs...)
			if qerr != nil {
				return qerr
			}
			defer rows.Close()
			for rows.Next() {
				var m memoryItem
				if serr := rows.Scan(&m.ID, &m.Title, &m.Domain, &m.Agent, &m.Preview, &m.CreatedAt); serr != nil {
					return serr
				}
				out = append(out, m)
			}
			return rows.Err()
		})
		if err != nil {
			writeInternalError(w, "memories list", err)
			return
		}
		writeJSON(w, http.StatusOK, withPageMeta(
			map[string]any{"domain": domain, "count": len(out), "pages": out},
			total, pg.Limit, pg.Offset, len(out)))
	}
}

// maxMemoryIDs limita o "select all" (ids são leves; teto defensivo pra não
// estourar o payload do dashboard com orgs gigantes). Acima disso, capped=true.
const maxMemoryIDs = 10000

// MemoryIDsHandler — GET /v1/memories/ids?domain=X&q=texto: retorna só os ids das
// pages que casam o filtro atual (mesmo filtro de MemoriesListHandler). Backing do
// botão "select all" do dashboard — selecionar tudo que foi pesquisado (ou tudo)
// sem paginar. `capped=true` sinaliza que o teto foi atingido (há mais).
func MemoryIDsHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		domain := r.URL.Query().Get("domain")
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		where, args := pageFilter(orgID, domain, q, "")

		ids := []int64{}
		err = tenant.RunWithTenantReadOnly(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			rows, qerr := tx.Query(r.Context(), fmt.Sprintf(
				`SELECT id FROM pages WHERE %s ORDER BY created_at DESC LIMIT %d`,
				where, maxMemoryIDs+1), args...)
			if qerr != nil {
				return qerr
			}
			defer rows.Close()
			for rows.Next() {
				var id int64
				if serr := rows.Scan(&id); serr != nil {
					return serr
				}
				ids = append(ids, id)
			}
			return rows.Err()
		})
		if err != nil {
			writeInternalError(w, "memory ids", err)
			return
		}
		capped := len(ids) > maxMemoryIDs
		if capped {
			ids = ids[:maxMemoryIDs]
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ids": ids, "count": len(ids), "capped": capped,
		})
	}
}
