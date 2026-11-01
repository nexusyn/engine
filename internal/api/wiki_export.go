package api

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/tenant"
)

type wikiExportItem struct {
	ID        int64
	Title     string
	Domain    string
	Project   string
	Content   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// WikiExportHandler — GET /v1/wiki/export?project=<slug>&format=tar.gz|json
// Projeta as memórias e documentações do PostgreSQL em arquivos Markdown estruturados
// prontos para uso em Obsidian ou gravação local, no estilo Karpathy LLM Wiki.
func WikiExportHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		if !requireOrgActive(w, r, pool, orgID) {
			return
		}

		project := strings.TrimSpace(r.URL.Query().Get("project"))
		format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
		if format == "" {
			format = "tar.gz"
		}

		var items []wikiExportItem
		err = tenant.RunWithTenantReadOnly(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			where, args := pageFilter(orgID, "", project, "", "")
			query := fmt.Sprintf(`
				SELECT id, title, domain, coalesce(project, ''), content, created_at, created_at
				FROM pages
				WHERE %s
				ORDER BY domain, title
			`, where)

			rows, qerr := tx.Query(r.Context(), query, args...)
			if qerr != nil {
				return qerr
			}
			defer rows.Close()

			for rows.Next() {
				var it wikiExportItem
				if err := rows.Scan(&it.ID, &it.Title, &it.Domain, &it.Project, &it.Content, &it.CreatedAt, &it.UpdatedAt); err != nil {
					return err
				}
				items = append(items, it)
			}
			return rows.Err()
		})

		if err != nil {
			writeInternalError(w, "wiki export query", err)
			return
		}

		if format == "json" {
			type jsonDoc struct {
				Path    string `json:"path"`
				Title   string `json:"title"`
				Domain  string `json:"domain"`
				Content string `json:"content"`
			}
			docs := make([]jsonDoc, 0, len(items))
			for _, it := range items {
				relPath := formatWikiPath(it)
				body := renderObsidianMarkdown(it)
				docs = append(docs, jsonDoc{
					Path:    relPath,
					Title:   it.Title,
					Domain:  it.Domain,
					Content: body,
				})
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"project": project,
				"count":   len(docs),
				"files":   docs,
			})
			return
		}

		// Default format = tar.gz
		w.Header().Set("Content-Type", "application/gzip")
		filename := "nexus-wiki"
		if project != "" {
			filename += "-" + project
		}
		filename += ".tar.gz"
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))

		gw := gzip.NewWriter(w)
		defer gw.Close()
		tw := tar.NewWriter(gw)
		defer tw.Close()

		for _, it := range items {
			relPath := formatWikiPath(it)
			body := renderObsidianMarkdown(it)
			bodyBytes := []byte(body)

			hdr := &tar.Header{
				Name:    relPath,
				Mode:    0644,
				Size:    int64(len(bodyBytes)),
				ModTime: it.UpdatedAt,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return
			}
			if _, err := tw.Write(bodyBytes); err != nil {
				return
			}
		}
	}
}

// formatWikiPath organiza em pastas temáticas: concepts/, decisions/, gotchas/, guidelines/, memories/
func formatWikiPath(it wikiExportItem) string {
	cleanTitle := sanitizeFilename(it.Title)
	folder := "memories"
	switch strings.ToLower(it.Domain) {
	case "wiki":
		folder = "concepts"
	case "guideline":
		folder = "guidelines"
	case "decision":
		folder = "decisions"
	case "gotcha":
		folder = "gotchas"
	}
	return fmt.Sprintf("%s/%s.md", folder, cleanTitle)
}

// renderObsidianMarkdown adiciona YAML frontmatter completo e formatação limpa
func renderObsidianMarkdown(it wikiExportItem) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("id: %d\n", it.ID))
	b.WriteString(fmt.Sprintf("title: %q\n", it.Title))
	b.WriteString(fmt.Sprintf("domain: %s\n", it.Domain))
	if it.Project != "" {
		b.WriteString(fmt.Sprintf("project: %s\n", it.Project))
	}
	b.WriteString(fmt.Sprintf("created_at: %s\n", it.CreatedAt.UTC().Format(time.RFC3339)))
	b.WriteString("---\n\n")

	b.WriteString("# " + strings.TrimSpace(it.Title) + "\n\n")
	b.WriteString(strings.TrimSpace(it.Content) + "\n")
	return b.String()
}

func sanitizeFilename(s string) string {
	s = strings.TrimSpace(s)
	replacer := strings.NewReplacer("/", "-", "\\", "-", ":", "-", "*", "-", "?", "", "\"", "", "<", "", ">", "", "|", "-")
	clean := replacer.Replace(s)
	if clean == "" {
		return "sem-titulo"
	}
	return clean
}
