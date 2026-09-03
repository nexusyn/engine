package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
)

// maxRequestBodyBytes é o teto de tamanho de request body aplicado ANTES de
// qualquer leitura em todo handler deste pacote que decodifica JSON (NEX-001:
// auditoria confirmou zero ocorrências de http.MaxBytesReader no repo — todo
// json.NewDecoder(r.Body).Decode lia o body inteiro na RAM antes de qualquer
// validação de tamanho, DoS de memória por body gigante). Configurável via
// NEXUS_MAX_REQUEST_BODY_BYTES; default 8 MiB cobre folgadamente qualquer
// payload legítimo (ingest/query/config). Valor ausente/inválido/≤0 cai no
// default.
var maxRequestBodyBytes = resolveMaxRequestBodyBytes()

func resolveMaxRequestBodyBytes() int64 {
	const def = int64(8 * 1024 * 1024) // 8 MiB
	v := os.Getenv("NEXUS_MAX_REQUEST_BODY_BYTES")
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// decodeJSONBody aplica http.MaxBytesReader (cap de tamanho ANTES de qualquer
// leitura) e decodifica o body JSON em v. Erro de corpo grande demais vira 413
// limpo — sem isso, `errors.As` não achou o *http.MaxBytesError e o cliente
// via um 400 genérico "unexpected EOF" (ou pior, o handler não tinha cap
// nenhum e o body inteiro ia pra RAM antes de qualquer decode falhar).
// Qualquer outro erro de parse vira 400. Fecha o body ao final. Retorna false
// com a resposta já escrita se decode falhou — o handler chamador só precisa
// dar `return`.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeJSONBodyLimit(w, r, v, maxRequestBodyBytes)
}

// decodeJSONBodyLimit é o decodeJSONBody com cap explícito — pros handlers cujo
// payload legítimo passa do default de 8 MiB (hoje só o /v1/import, que recebe
// o arquivo do export em lotes).
func decodeJSONBodyLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	defer func() { _ = r.Body.Close() }()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body too large (max %d bytes)", maxErr.Limit))
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return false
	}
	return true
}
