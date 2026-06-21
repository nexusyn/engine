package search

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Constantes do canal entity-match (Sprint cleanroom, item B — boost
// atenuado). Substituem o boost FLAT 1.5× por um peso por-entidade que penaliza
// entidades pouco discriminativas (alto grau no grafo).
const (
	// entityMatchThreshold é a similaridade trigram mínima entre name e query
	// pra a entidade contar como match. 0.3 (NÃO 0.5): similarity() compara a
	// string INTEIRA, então uma pergunta NL longa ("How many tanks do I have,
	// including the one I set up for my friend's kid?") tem sim trigram baixa
	// contra um name curto ("tank","Amazonia") mesmo quando ele aparece literal.
	// 0.5 sufocava o canal de recall de entidades justo nas queries de agregação
	// multi-sessão que mais dependem dele (undercount 2 vs 3 tanques etc.). A
	// disciplina de B vem da ATENUAÇÃO por grau + cap, não de um threshold alto.
	entityMatchThreshold = 0.3

	// entityMatchCap limita quantas entidades uma query injeta no canal. 8 (era
	// 20) evita que uma query verbosa arraste dezenas de entidades fracas.
	entityMatchCap = 8

	// entityDegreeDamping é o coeficiente da atenuação por grau. O peso de uma
	// entidade no canal é sim ÷ (1 + k·(grau−1)²): entidade ligada a MUITAS
	// pages/edges (alto grau, pouco discriminativa) pesa menos. k pequeno =
	// atenuação suave (grau 1→sem corte; grau 30→~×0.54). Espelha o IDF-like
	// "rare entities matter more" da literatura, mantendo o canal rank-based no RRF.
	entityDegreeDamping = 0.001
)

// entityMatchSearch retorna chunks ranked por overlap com entities extraídas
// das mesmas pages. Sprint 2.5 — 3º sinal RRF além de vector + FTS.
//
// Sprint 3.2: aceita `asOf` opcional pra time-travel. Quando passado,
// entities e edges são filtradas pelo snapshot temporal (valid_from/valid_to
// covering o asOf), e chunks são filtrados por pages.created_at <= asOf.
//
// cleanroom (item B): o ranking intra-canal agora usa peso atenuado por
// grau em vez de contagem crua de hits. Cada entidade matched contribui
// sim ÷ (1 + entityDegreeDamping·(grau−1)²) ao score da page; chunks são
// ordenados pela soma desses pesos. O boost de canal (EntityMatchWeight) segue
// aplicado no RRF — aqui só melhoramos a ORDEM dentro do canal.
//
// Estratégia (sem chamada LLM, só SQL):
//  1. Achar entities cujo NAME tem similarity trgm > entityMatchThreshold com a
//     query, OU cujo aliases contém uma palavra da query (case-insensitive),
//     limitado a entityMatchCap entidades (as mais similares). Quando asOf
//     setado, restringe a entities válidas naquele timestamp.
//  2. Para cada entity, calcular o GRAU (nº de edges current em que participa)
//     e o peso atenuado.
//  3. Achar PAGES fonte dessas entities (edges), somando os pesos atenuados.
//     Quando asOf setado, restringe a edges válidas naquele timestamp.
//  4. Trazer CHUNKS dessas pages, ranked pela soma de peso (whits) DESC.
//     Quando asOf setado, filtra chunks cuja page foi criada até essa data.
//
// Falha graceful: erro de DB retorna nil + erro. Caller (runHybrid) trata o
// sinal como opcional.
func entityMatchSearch(ctx context.Context, tx pgx.Tx, query string, limit int, domain string, asOf *time.Time) ([]int64, error) {
	if query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}

	// Constrói filtros temporais como SQL snippets — vazios quando asOf nil.
	entValidFilter := "AND e.valid_to IS NULL" // default: current
	edgeValidFilter := "AND edge.valid_to IS NULL"
	pageCreatedFilter := ""
	if asOf != nil {
		entValidFilter = "AND e.valid_from <= $2 AND (e.valid_to IS NULL OR e.valid_to > $2)"
		edgeValidFilter = "AND edge.valid_from <= $2 AND (edge.valid_to IS NULL OR edge.valid_to > $2)"
		pageCreatedFilter = "AND p.created_at <= $2"
	}

	// $1 = query, $2 = asOf (se setado), $N = domain/limit (depende).
	args := []any{query}
	paramN := 2
	if asOf != nil {
		args = append(args, *asOf)
		paramN = 3
	}

	// ents: entidades match + similaridade (alias match conta como 1.0), capadas.
	// ent_deg: junta o grau (edges current da entidade) e o peso atenuado.
	// matched_pages: soma o peso atenuado por page de origem.
	sql := fmt.Sprintf(`
		WITH ents AS (
			-- Perf: o operador %% usa o índice GIN trgm entities_name_trgm_idx; o
			-- antigo similarity()>thr OR EXISTS(alias) forçava seq scan de todas as
			-- entities (~90ms). Ramo do nome (índice) + ramo de alias (seq, mas só
			-- nas entities com alias), via UNION. threshold do %% = GUC
			-- pg_trgm.similarity_threshold (0.3 = entityMatchThreshold).
			SELECT id, max(sim) AS sim FROM (
				SELECT e.id, similarity(e.name, $1) AS sim
				FROM entities e
				WHERE e.name %% $1 %s
				UNION ALL
				SELECT e.id, 1.0 AS sim
				FROM entities e
				WHERE EXISTS (SELECT 1 FROM unnest(e.aliases) a WHERE position(lower(a) IN lower($1)) > 0) %s
			) u
			GROUP BY id
			ORDER BY sim DESC
			LIMIT %d
		),
		ent_deg AS (
			SELECT en.id, en.sim,
			       (SELECT count(*) FROM edges d
			         WHERE (d.from_entity_id = en.id OR d.to_entity_id = en.id)
			           AND d.valid_to IS NULL) AS degree
			FROM ents en
		),
		matched_pages AS (
			SELECT edge.source_page_id AS page_id,
			       sum(ed.sim / (1.0 + %g * power(GREATEST(ed.degree - 1, 0), 2))) AS whits
			FROM edges edge
			JOIN ent_deg ed ON ed.id = edge.from_entity_id OR ed.id = edge.to_entity_id
			WHERE edge.source_page_id IS NOT NULL
			  %s
			GROUP BY edge.source_page_id
		)
		SELECT c.id
		FROM chunks c
		JOIN matched_pages mp ON mp.page_id = c.page_id
		JOIN pages p ON p.id = c.page_id
		WHERE TRUE
		  %s`, entValidFilter, entValidFilter, entityMatchCap, entityDegreeDamping, edgeValidFilter, pageCreatedFilter)

	if domain != "" {
		args = append(args, domain)
		sql += fmt.Sprintf(" AND p.domain = $%d", paramN)
		paramN++
	}
	args = append(args, limit)
	sql += fmt.Sprintf(" ORDER BY mp.whits DESC, c.position ASC LIMIT $%d", paramN)

	rows, err := tx.Query(ctx, strings.TrimSpace(sql), args...)
	if err != nil {
		return nil, fmt.Errorf("entity match search: %w", err)
	}
	defer rows.Close()

	out := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("entity match scan: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
