package auth

import (
	"net/http/httptest"
	"testing"

	"golang.org/x/time/rate"
)

func TestAuthFailLimiter_BloqueiaAposBurst(t *testing.T) {
	l := &authFailLimiter{m: make(map[string]*rate.Limiter)}
	ip := "1.2.3.4"
	if l.blocked(ip) {
		t.Fatal("IP novo não devia estar bloqueado")
	}
	for i := 0; i < 20; i++ {
		l.fail(ip) // esgota o burst de 20
	}
	if !l.blocked(ip) {
		t.Fatal("após 20 falhas o IP devia estar bloqueado")
	}
	if l.blocked("9.9.9.9") {
		t.Fatal("outro IP não devia ser afetado pelo bloqueio do primeiro")
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:55555"
	r.Header.Set("X-Forwarded-For", "203.0.113.5, 10.0.0.1")
	if got := clientIP(r); got != "203.0.113.5" {
		t.Errorf("X-Forwarded-For: got %q, want 203.0.113.5", got)
	}

	r2 := httptest.NewRequest("GET", "/", nil)
	r2.RemoteAddr = "10.0.0.1:55555"
	if got := clientIP(r2); got != "10.0.0.1" {
		t.Errorf("RemoteAddr: got %q, want 10.0.0.1", got)
	}
}
