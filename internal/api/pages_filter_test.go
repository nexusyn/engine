package api

import (
	"reflect"
	"testing"
)

// pageFilter monta SQL dinâmico — a numeração dos placeholders ($1,$2,…) e o
// alias têm que casar com os args, senão o pgx erra em runtime. Estes testes
// fixam exatamente o WHERE e os args por combinação de domain/q.
func TestPageFilter(t *testing.T) {
	const org = int64(2)
	cases := []struct {
		name      string
		domain    string
		q         string
		alias     string
		wantWhere string
		wantArgs  []any
	}{
		{
			name:      "só org, sem alias",
			wantWhere: "organization_id = $1 AND valid_to IS NULL",
			wantArgs:  []any{org},
		},
		{
			name:      "só org, alias p",
			alias:     "p",
			wantWhere: "p.organization_id = $1 AND p.valid_to IS NULL",
			wantArgs:  []any{org},
		},
		{
			name:      "domain",
			domain:    "wiki",
			wantWhere: "organization_id = $1 AND valid_to IS NULL AND domain = $2",
			wantArgs:  []any{org, "wiki"},
		},
		{
			name:      "q (ILIKE title/content no mesmo placeholder)",
			q:         "billing",
			wantWhere: "organization_id = $1 AND valid_to IS NULL AND (title ILIKE $2 OR content ILIKE $2)",
			wantArgs:  []any{org, "%billing%"},
		},
		{
			name:      "domain + q, alias p (q vira $3)",
			domain:    "lesson",
			q:         "rerank",
			alias:     "p",
			wantWhere: "p.organization_id = $1 AND p.valid_to IS NULL AND p.domain = $2 AND (p.title ILIKE $3 OR p.content ILIKE $3)",
			wantArgs:  []any{org, "lesson", "%rerank%"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			where, args := pageFilter(org, tc.domain, tc.q, tc.alias)
			if where != tc.wantWhere {
				t.Errorf("where:\n got %q\nwant %q", where, tc.wantWhere)
			}
			if !reflect.DeepEqual(args, tc.wantArgs) {
				t.Errorf("args: got %#v, want %#v", args, tc.wantArgs)
			}
		})
	}
}

func TestParseIDList(t *testing.T) {
	cases := []struct {
		in   string
		want []int64
	}{
		{"", nil},
		{"   ", nil},
		{"1,2,3", []int64{1, 2, 3}},
		{" 1 , 2 ,3 ", []int64{1, 2, 3}},
		{"1,abc,3", []int64{1, 3}}, // ignora não-numérico
		{"0,-1,5", []int64{5}},     // ignora <= 0
		{"42", []int64{42}},
	}
	for _, tc := range cases {
		got := parseIDList(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseIDList(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}
