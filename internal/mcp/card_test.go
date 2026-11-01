package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServerCardHandler(t *testing.T) {
	h := ServerCardHandler("0.4.0")
	req := httptest.NewRequest("GET", "/.well-known/mcp/server-card.json", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w.Code)
	}

	var card ServerCard
	if err := json.Unmarshal(w.Body.Bytes(), &card); err != nil {
		t.Fatalf("failed to parse json: %v", err)
	}

	if card.ServerInfo.Name != "nexusyn" {
		t.Errorf("expected name 'nexusyn', got %q", card.ServerInfo.Name)
	}
	if card.ServerInfo.Version != "0.4.0" {
		t.Errorf("expected version '0.4.0', got %q", card.ServerInfo.Version)
	}
	if len(card.Tools) != 7 {
		t.Errorf("expected 7 tools, got %d", len(card.Tools))
	}
}
