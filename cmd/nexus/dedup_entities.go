package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/config"
)

// runDedupEntities funde entidades duplicadas de UMA org (Fase 5 GraphRAG). O
// extractor faz UPSERT por slug exato, então "Nexusyn"/"NEXUSYN"/"nexusyn-billing…"
// viram nós separados, fragmentando o grafo. Aqui: blocking por trigram (índice
// GIN) → verificação por similaridade → union-find → merge (canônico = maior grau;
// move edges, mescla aliases, recalcula degree).
//
// DESTRUTIVO (N3): apaga as entidades duplicadas e reescreve edges. Exige --yes;
// --dry-run lista os grupos sem alterar. Guardas anti-over-merge: mesmo kind,
// nome ≥ minLen, e NUNCA funde "irmãos numerados" (Sprint 1 vs Sprint 2, M1 vs M2).
func runDedupEntities(args []string) error {
	fs := flag.NewFlagSet("dedup-entities", flag.ContinueOnError)
	org := fs.Int64("org", 0, "organization id (obrigatório)")
	threshold := fs.Float64("threshold", 0.85, "similaridade trigram mínima p/ fundir (0-1)")
	minLen := fs.Int("min-len", 3, "comprimento mínimo do nome p/ considerar (evita siglas curtas)")
	yes := fs.Bool("yes", false, "confirma o merge destrutivo")
	dryRun := fs.Bool("dry-run", false, "lista os grupos candidatos, não altera nada")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *org <= 0 {
		return fmt.Errorf("dedup-entities: --org é obrigatório")
	}

	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config load: %w", err)
	}
	pool, err := pgxpool.New(ctx, cfg.DB.AdminOrDefault())
	if err != nil {
		return fmt.Errorf("db pool: %w", err)
	}
	defer pool.Close()

	// 1. Pares candidatos: trigram acima do threshold, MESMO kind, nomes ≥ minLen.
	//    a.name % b.name usa o índice GIN trgm pro join (blocking barato).
	type pair struct {
		aID, bID     int64
		aName, bName string
		aDeg, bDeg   int
	}
	rows, err := pool.Query(ctx, `
		SELECT a.id, a.name, a.degree, b.id, b.name, b.degree
		FROM entities a
		JOIN entities b
		  ON a.organization_id = $1 AND b.organization_id = $1
		 AND a.kind = b.kind AND a.id < b.id
		 AND a.valid_to IS NULL AND b.valid_to IS NULL
		 AND a.name % b.name
		WHERE char_length(a.name) >= $3 AND char_length(b.name) >= $3
		  AND similarity(a.name, b.name) >= $2`, *org, *threshold, *minLen)
	if err != nil {
		return fmt.Errorf("query candidates: %w", err)
	}
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.aID, &p.aName, &p.aDeg, &p.bID, &p.bName, &p.bDeg); err != nil {
			rows.Close()
			return fmt.Errorf("scan pair: %w", err)
		}
		if !numbersDiffer(p.aName, p.bName) {
			pairs = append(pairs, p)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rows: %w", err)
	}

	// 2. Union-find sobre os pares → grupos a fundir.
	uf := newUnionFind()
	deg := map[int64]int{}
	name := map[int64]string{}
	for _, p := range pairs {
		uf.union(p.aID, p.bID)
		deg[p.aID], deg[p.bID] = p.aDeg, p.bDeg
		name[p.aID], name[p.bID] = p.aName, p.bName
	}
	groups := uf.groups() // root → membros

	fmt.Printf("dedup-entities org %d: %d pares, %d grupos a fundir (threshold %.2f)\n",
		*org, len(pairs), len(groups), *threshold)
	if len(groups) == 0 {
		return nil
	}

	// Escolhe canônico = maior grau (desempate: menor id) e lista.
	type merge struct {
		canonical int64
		dups      []int64
	}
	var merges []merge
	for _, members := range groups {
		canon := members[0]
		for _, m := range members {
			if deg[m] > deg[canon] || (deg[m] == deg[canon] && m < canon) {
				canon = m
			}
		}
		var dups []int64
		for _, m := range members {
			if m != canon {
				dups = append(dups, m)
			}
		}
		merges = append(merges, merge{canonical: canon, dups: dups})
		if *dryRun {
			names := make([]string, 0, len(dups))
			for _, d := range dups {
				names = append(names, name[d])
			}
			fmt.Printf("  «%s» (deg %d) ← %s\n", name[canon], deg[canon], strings.Join(names, " | "))
		}
	}

	if *dryRun {
		fmt.Println("(dry-run — nada alterado)")
		return nil
	}
	if !*yes {
		return fmt.Errorf("operação destrutiva — re-rode com --yes para confirmar (backup antes!)")
	}

	// 3. Executa os merges. Cada grupo numa transação: move edges, mescla aliases,
	//    apaga duplicatas, dropa self-loops, recalcula degree do canônico.
	merged, edgesMoved := 0, 0
	for _, m := range merges {
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			for _, d := range m.dups {
				t1, e := tx.Exec(ctx, `UPDATE edges SET from_entity_id=$1 WHERE from_entity_id=$2 AND organization_id=$3`, m.canonical, d, *org)
				if e != nil {
					return e
				}
				t2, e := tx.Exec(ctx, `UPDATE edges SET to_entity_id=$1 WHERE to_entity_id=$2 AND organization_id=$3`, m.canonical, d, *org)
				if e != nil {
					return e
				}
				edgesMoved += int(t1.RowsAffected() + t2.RowsAffected())
				// absorve o nome+aliases da duplicata no canônico
				if _, e := tx.Exec(ctx, `
					UPDATE entities SET aliases = (
						SELECT array_agg(DISTINCT x) FROM unnest(
							coalesce(c.aliases,'{}') || coalesce(d.aliases,'{}') || ARRAY[d.name]
						) x WHERE x <> c.name
					), attributes = coalesce(d.attributes,'{}'::jsonb) || coalesce(c.attributes,'{}'::jsonb)
					FROM entities c, entities d
					WHERE entities.id = $1 AND c.id = $1 AND d.id = $2`, m.canonical, d); e != nil {
					return e
				}
				if _, e := tx.Exec(ctx, `DELETE FROM entities WHERE id=$1 AND organization_id=$2`, d, *org); e != nil {
					return e
				}
			}
			// self-loops criados pelo merge (A-B onde A e B fundiram) não fazem sentido
			if _, e := tx.Exec(ctx, `DELETE FROM edges WHERE from_entity_id=to_entity_id AND organization_id=$1`, *org); e != nil {
				return e
			}
			// recalcula degree do canônico
			if _, e := tx.Exec(ctx, `
				UPDATE entities SET degree = (
					SELECT count(*) FROM edges x
					WHERE (x.from_entity_id=$1 OR x.to_entity_id=$1) AND x.valid_to IS NULL
				) WHERE id=$1`, m.canonical); e != nil {
				return e
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("merge grupo canônico %d: %w", m.canonical, err)
		}
		merged += len(m.dups)
	}

	slog.Info("dedup-entities done", "org", *org, "entities_merged", merged, "groups", len(merges), "edges_moved", edgesMoved)
	fmt.Printf("fundidas %d entidades em %d grupos; %d edges movidas.\n", merged, len(merges), edgesMoved)
	return nil
}

// numbersDiffer: o MULTISET de números dos dois nomes difere → entidades
// DISTINTAS, NUNCA fundir (Sprint 6 vs Sprint 6.6 = [6] vs [6,6]; 2026-06 vs
// 2026-06-06; 34/34 vs 34%; v1 vs v2; M1 vs M2). Reordenações sem números, e a
// MESMA data em formatos diferentes (2026-05-26 vs 26/05/2026 = mesmo multiset),
// passam. É a guarda anti-over-merge central, derivada de falsos-positivos reais.
var numRe = regexp.MustCompile(`\d+`)

func numbersDiffer(a, b string) bool {
	na := numRe.FindAllString(a, -1)
	nb := numRe.FindAllString(b, -1)
	sort.Strings(na)
	sort.Strings(nb)
	return !slices.Equal(na, nb)
}

// union-find simples (path-compression) sobre entity ids.
type unionFind struct{ parent map[int64]int64 }

func newUnionFind() *unionFind { return &unionFind{parent: map[int64]int64{}} }

func (u *unionFind) find(x int64) int64 {
	if _, ok := u.parent[x]; !ok {
		u.parent[x] = x
		return x
	}
	for u.parent[x] != x {
		u.parent[x] = u.parent[u.parent[x]]
		x = u.parent[x]
	}
	return x
}

func (u *unionFind) union(a, b int64) {
	ra, rb := u.find(a), u.find(b)
	if ra != rb {
		u.parent[ra] = rb
	}
}

func (u *unionFind) groups() map[int64][]int64 {
	g := map[int64][]int64{}
	for x := range u.parent {
		r := u.find(x)
		g[r] = append(g[r], x)
	}
	return g
}
