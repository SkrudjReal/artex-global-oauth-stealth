package db

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"net/url"
)

func TestOAuthMigrationAndProfileBoundaries(t *testing.T) {
	dsn := testDSN(t)
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err := admin.PingContext(t.Context()); err != nil {
		t.Fatal("test PostgreSQL unavailable")
	}
	schema := fmt.Sprintf("oauth_test_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(t.Context(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
	cfg, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("invalid test DSN")
	}
	query := cfg.Query()
	query.Set("search_path", schema)
	cfg.RawQuery = query.Encode()
	isolated := cfg.String()
	// A second Open applies the idempotent migration over existing profile data.
	d, err := Open(isolated)
	if err != nil {
		t.Fatal(err)
	}
	var activeSchema string
	if err := d.QueryRow(`SELECT current_schema()`).Scan(&activeSchema); err != nil || activeSchema != schema {
		d.Close()
		t.Fatal("test schema isolation failed")
	}
	apiID, err := d.SaveProfile(&LLMProfile{Name: "API", Format: "openai", Model: "model", APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	d, err = Open(isolated)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	regular, err := d.ProfileByID(apiID)
	if err != nil || regular.APIKey != "test-key" {
		t.Fatal("migration changed API Key profile")
	}
	for _, kind := range []string{"codex-oauth", "agy-oauth"} {
		id, err := d.SaveProfile(&LLMProfile{Name: kind, Format: kind, Model: "model"})
		if err != nil {
			t.Fatal(err)
		}
		p, err := d.ProfileByID(id)
		if err != nil || !p.OAuth() || p.APIKey != "" || (!p.SupportsAgentTools() && !p.PoolExclude) {
			t.Fatalf("OAuth profile boundary: %v", err)
		}
		if err := d.SetActiveProfile(id); (err == nil) == !p.SupportsAgentTools() {
			t.Fatal("External Agent made global default")
		}
		tx, err := d.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := lockLLMProfileForReference(tx, &id); (err == nil) == !p.SupportsAgentTools() {
			t.Fatal("External Agent accepted for agent/conversation binding")
		}
		tx.Rollback()
		p.APIKey = "must-not-be-stored"
		if _, err := d.SaveProfile(p); err == nil {
			t.Fatal("OAuth profile accepted credentials")
		}
	}
	pool, err := d.PoolProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(pool) != 3 || pool[0].Format != "agy-oauth" || pool[1].ID != apiID {
		t.Fatal("OAuth profile entered API Key pool")
	}
}
