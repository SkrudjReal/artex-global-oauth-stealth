package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/Autumn-27/artex/db"
)

type bundledMCP struct {
	db.MCPServer
	Agents []string `json:"agents"`
}

func readBundledMCPs(path string) ([]bundledMCP, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []bundledMCP
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if (entry.Name != "cveintel" && entry.Name != "hexstrike") || entry.Transport != "stdio" || !filepath.IsAbs(entry.Command) {
			return nil, fmt.Errorf("invalid bundled MCP %q", entry.Name)
		}
	}
	return entries, nil
}

// Only new defaults receive bindings; operator edits and disabled servers survive restarts.
func seedBundledMCP(pg *db.DB, entry bundledMCP) error {
	tx, err := pg.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRow(`INSERT INTO mcp_servers(name,transport,command,args,env,enabled)
	 VALUES($1,'stdio',$2,$3,'{}',$4) ON CONFLICT(name) DO NOTHING RETURNING id`,
		entry.Name, entry.Command, string(entry.Args), entry.Enabled).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, key := range entry.Agents {
		if _, err := tx.Exec(`INSERT INTO agent_visibility(agent_id,resource_kind,resource_id,enabled)
		 SELECT id,'mcp',$1,true FROM agents WHERE key=$2 ON CONFLICT DO NOTHING`, id, key); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Server) prepareBundledMCPs() {
	path := os.Getenv("ARTEX_MCP_DEFAULTS")
	if path == "" {
		path = filepath.Join(filepath.Dir(s.skillDir), "integrations", "mcp-defaults.json")
	}
	entries, err := readBundledMCPs(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		log.Printf("[mcp] bundled defaults: %v", err)
		return
	}
	for _, entry := range entries {
		if err := seedBundledMCP(s.m.pg, entry); err != nil {
			log.Printf("[mcp] seed %s: %v", entry.Name, err)
		}
	}
}

func (s *Server) warmBundledMCPs() {
	servers, err := s.m.pg.ListMCP()
	if err != nil {
		return
	}
	for _, m := range servers {
		if !m.Enabled || (m.Name != "cveintel" && m.Name != "hexstrike") {
			continue
		}
		ctx, cancel := context.WithTimeout(s.ctx, 90*time.Second)
		if err := s.discoverAndCacheMCP(ctx, m); err != nil {
			log.Printf("[mcp] startup %s: %v", m.Name, err)
		}
		cancel()
	}
}
