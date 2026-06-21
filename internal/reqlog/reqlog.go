// Package reqlog mantém um log em memória (ring buffer) das últimas requisições
// à API, por org. Backing da tela Requests do dashboard. In-memory de propósito:
// observabilidade leve, sem custo de escrita no DB por request nem migration;
// é "atividade recente" (perde no restart), não auditoria persistente.
package reqlog

import (
	"net/http"
	"sync"
	"time"

	"github.com/nexusyn/engine/internal/tenant"
)

// Entry é uma requisição registrada.
type Entry struct {
	OrgID     int64     `json:"-"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Status    int       `json:"status"`
	LatencyMs int64     `json:"latency_ms"`
	At        time.Time `json:"at"`
}

const maxEntries = 1000

var (
	mu  sync.Mutex
	buf []Entry
)

func record(e Entry) {
	mu.Lock()
	defer mu.Unlock()
	buf = append(buf, e)
	if len(buf) > maxEntries {
		buf = buf[len(buf)-maxEntries:]
	}
}

// Recent retorna entries da org, mais novas primeiro: pula as `offset` mais
// recentes e devolve até `limit` a partir daí (paginação sobre o ring buffer).
func Recent(orgID int64, limit, offset int) []Entry {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Entry, 0, limit)
	skipped := 0
	for i := len(buf) - 1; i >= 0 && len(out) < limit; i-- {
		if buf[i].OrgID != orgID {
			continue
		}
		if skipped < offset {
			skipped++
			continue
		}
		out = append(out, buf[i])
	}
	return out
}

// Count retorna quantas entries do ring buffer pertencem à org (total p/ paginação).
func Count(orgID int64) int {
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for i := range buf {
		if buf[i].OrgID == orgID {
			n++
		}
	}
	return n
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

// O SSE do /v1/query/stream exige http.Flusher; o embed não propaga a
// interface, então sem estes métodos o stream morre com "streaming não
// suportado pelo writer" atrás deste middleware.
var _ http.Flusher = (*statusRecorder)(nil)

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap permite http.NewResponseController alcançar o writer original.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Middleware registra cada request (após o auth, pra ter a org no contexto).
// Não registra a própria /v1/requests pra não poluir.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			return // sem org (ex: auth falhou) → não loga
		}
		record(Entry{
			OrgID:     orgID,
			Method:    r.Method,
			Path:      r.URL.Path,
			Status:    rec.status,
			LatencyMs: time.Since(start).Milliseconds(),
			At:        time.Now().UTC(),
		})
	})
}
