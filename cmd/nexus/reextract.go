package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/config"
)

// runReextract reconstrói o knowledge graph de UMA org do zero: apaga as
// entities+edges existentes e re-enfileira a extração de todas as pages já
// processadas. Necessário pós-Fase 2 (confidence): o grafo legado tem todas as
// arestas marcadas extracted/1.0 (o backfill não tinha como inferir confiança);
// só a re-extração com o worker novo atribui confidence REAL por aresta (e o
// vocabulário canônico de relações na origem). Habilita o gate da graph-expansion.
//
// DESTRUTIVO (N3): apaga o grafo da org. Exige --yes. Use --dry-run pra inventário.
// Idempotente no fim: os jobs extract_entities reconstroem; rerun é seguro.
// Usa ADMIN_DATABASE_URL (bypassa RLS) e filtra por organization_id explícito.
func runReextract(args []string) error {
	fs := flag.NewFlagSet("reextract", flag.ContinueOnError)
	org := fs.Int64("org", 0, "organization id (obrigatório)")
	yes := fs.Bool("yes", false, "confirma a operação destrutiva (apaga o grafo da org)")
	dryRun := fs.Bool("dry-run", false, "só mostra o inventário, não altera nada")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *org <= 0 {
		return fmt.Errorf("reextract: --org é obrigatório (ex: --org 2)")
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

	// Inventário.
	var nEnt, nEdge, nPages int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM entities WHERE organization_id=$1`, *org).Scan(&nEnt); err != nil {
		return fmt.Errorf("count entities: %w", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM edges WHERE organization_id=$1`, *org).Scan(&nEdge); err != nil {
		return fmt.Errorf("count edges: %w", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pages
		WHERE organization_id=$1 AND entities_extracted_at IS NOT NULL AND content <> ''`, *org).Scan(&nPages); err != nil {
		return fmt.Errorf("count pages: %w", err)
	}
	fmt.Printf("reextract org %d: APAGA %d entities + %d edges; RE-EXTRAI %d pages\n", *org, nEnt, nEdge, nPages)

	if *dryRun {
		fmt.Println("(dry-run — nada alterado)")
		return nil
	}
	if !*yes {
		return fmt.Errorf("operação destrutiva — re-rode com --yes para confirmar (backup antes!)")
	}

	// Captura os page_ids alvo ANTES de resetar a flag.
	rows, err := pool.Query(ctx, `
		SELECT id FROM pages
		WHERE organization_id=$1 AND entities_extracted_at IS NOT NULL AND content <> ''
		ORDER BY id`, *org)
	if err != nil {
		return fmt.Errorf("select target pages: %w", err)
	}
	var pageIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan page id: %w", err)
		}
		pageIDs = append(pageIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rows: %w", err)
	}

	// 1. Limpa o grafo + reseta a flag de extração — tudo numa transação.
	//    edges ANTES de entities (FK). Reset só das pages que serão re-extraídas.
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM edges WHERE organization_id=$1`, *org); err != nil {
			return fmt.Errorf("delete edges: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM entities WHERE organization_id=$1`, *org); err != nil {
			return fmt.Errorf("delete entities: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE pages SET entities_extracted_at = NULL
			WHERE organization_id=$1 AND entities_extracted_at IS NOT NULL AND content <> ''`, *org); err != nil {
			return fmt.Errorf("reset extracted flag: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("reextract clean tx: %w", err)
	}
	slog.Info("reextract: grafo apagado", "org", *org, "entities", nEnt, "edges", nEdge)

	// 2. Re-enfileira a extração via INSERT direto na river_job — o River valida o
	//    kind no InsertMany do client, então um client CLI (sem o Workers bundle do
	//    worker.go) rejeita "extract_entities". Inserir o job pela tabela é o mesmo
	//    que o River faria; o nexus-worker (queue default) consome os available.
	tag, err := pool.Exec(ctx, `
		INSERT INTO river_job (kind, queue, state, max_attempts, priority, args, metadata, tags, scheduled_at, created_at)
		SELECT 'extract_entities', 'default', 'available'::river_job_state, 3, 1,
		       jsonb_build_object('page_id', id, 'organization_id', $1::bigint),
		       '{}'::jsonb, '{}'::varchar[], now(), now()
		FROM pages WHERE id = ANY($2::bigint[])`, *org, pageIDs)
	if err != nil {
		return fmt.Errorf("enqueue extract jobs: %w", err)
	}
	enq := tag.RowsAffected()

	slog.Info("reextract done", "org", *org, "jobs_enqueued", enq)
	fmt.Printf("grafo apagado; %d jobs extract_entities enfileirados (queue default).\n", enq)
	fmt.Println("acompanhe: SELECT count(*) FROM pages WHERE organization_id=" +
		fmt.Sprint(*org) + " AND entities_extracted_at IS NULL AND content<>'';")
	return nil
}
