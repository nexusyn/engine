package api

import (
	"net/http"
	"strconv"
	"strings"
)

// parseIDList parseia uma csv de ids ("1,2,3") em []int64, ignorando vazios e
// não-numéricos. Usado pelo export por seleção (?ids=). Dedup não é necessário
// (id = ANY tolera repetidos).
func parseIDList(csv string) []int64 {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	var ids []int64
	for _, part := range strings.Split(csv, ",") {
		if n, e := strconv.ParseInt(strings.TrimSpace(part), 10, 64); e == nil && n > 0 {
			ids = append(ids, n)
		}
	}
	return ids
}

// pageParams contém limit/offset normalizados a partir da query string.
type pageParams struct {
	Limit  int
	Offset int
}

// parsePageParams lê ?limit= e ?offset= com default e cap. limit ausente/<=0 →
// def; limit > max → max. offset ausente/<0 → 0. Retrocompatível: quem manda só
// ?limit= continua funcionando; ?offset= destrava a paginação real (antes os
// lists só tinham um teto e o resto das memórias/entidades ficava inacessível).
func parsePageParams(r *http.Request, def, max int) pageParams {
	limit := def
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			limit = n
		}
	}
	if limit > max {
		limit = max
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			offset = n
		}
	}
	return pageParams{Limit: limit, Offset: offset}
}

// withPageMeta acrescenta os campos de paginação ao corpo de resposta de uma
// lista (in-place) e o devolve. Vai junto com as chaves já existentes (count +
// a lista) pra não quebrar consumidores antigos. total = total de linhas que
// casam o filtro (ignorando limit/offset); has_more sinaliza se há mais páginas.
func withPageMeta(body map[string]any, total, limit, offset, returned int) map[string]any {
	body["total"] = total
	body["limit"] = limit
	body["offset"] = offset
	body["has_more"] = offset+returned < total
	return body
}
