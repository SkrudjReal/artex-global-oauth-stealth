package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestReadBundledMCPs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "defaults.json")
	for _, tc := range []struct {
		input string
		valid bool
	}{
		{`[{"name":"cveintel","transport":"stdio","command":"/usr/bin/python3","args":[]}]`, true},
		{`[{"name":"hexstrike","transport":"http","command":"/usr/bin/python3"}]`, false},
		{`[{"name":"other","transport":"stdio","command":"/usr/bin/python3"}]`, false},
		{`[{"name":"cveintel","transport":"stdio","command":"python3"}]`, false},
		{`{`, false},
	} {
		if err := os.WriteFile(path, []byte(tc.input), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := readBundledMCPs(path)
		if (err == nil) != tc.valid {
			t.Fatalf("input=%s error=%v", tc.input, err)
		}
	}
}

func TestBundledMCPPreservesOperatorConfiguration(t *testing.T) {
	dsn := os.Getenv("ARTEX_MCP_TEST_DSN")
	if dsn == "" {
		t.Skip("requires an isolated ARTEX_MCP_TEST_DSN")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	entry := bundledMCP{MCPServer: db.MCPServer{Name: "cveintel", Transport: "stdio", Command: "/test/python", Args: json.RawMessage(`[]`), Enabled: true}, Agents: []string{"mainagent", "planner", "worker"}}
	if err := seedBundledMCP(pg, entry); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := pg.QueryRow(`SELECT id FROM mcp_servers WHERE name='cveintel'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pg.QueryRow(`SELECT count(*) FROM agent_visibility WHERE resource_kind='mcp' AND resource_id=$1`, id).Scan(&count); err != nil || count != 3 {
		t.Fatalf("bindings=%d error=%v", count, err)
	}
	if _, err := pg.Exec(`UPDATE mcp_servers SET command='/custom/python',enabled=false,env='{"PRIVATE_TOKEN":"sentinel"}' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(`DELETE FROM agent_visibility WHERE resource_kind='mcp' AND resource_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := seedBundledMCP(pg, entry); err != nil {
		t.Fatal(err)
	}
	var command, env string
	var enabled bool
	if err := pg.QueryRow(`SELECT command,enabled,env::text FROM mcp_servers WHERE id=$1`, id).Scan(&command, &enabled, &env); err != nil {
		t.Fatal(err)
	}
	if command != "/custom/python" || enabled || env != `{"PRIVATE_TOKEN": "sentinel"}` {
		t.Fatalf("operator configuration overwritten")
	}
	if err := pg.QueryRow(`SELECT count(*) FROM agent_visibility WHERE resource_kind='mcp' AND resource_id=$1`, id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("bindings restored: %d error=%v", count, err)
	}
}
