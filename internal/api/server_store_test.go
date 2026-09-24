package api

import (
	"strings"
	"testing"

	"prism-2api/internal/config"
)

func TestNewServerRequiresPostgres(t *testing.T) {
	_, err := NewServer(&config.Config{
		CredentialDir: t.TempDir(),
		MockMode:      false,
	})
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("want DATABASE_URL required, got %v", err)
	}
}
