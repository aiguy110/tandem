package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRegenSchemaFixture(t *testing.T) {
	if os.Getenv("REGEN_SCHEMA") == "" {
		t.Skip("set REGEN_SCHEMA=1 to regenerate")
	}
	s, _ := openTestStore(t)
	type schemaRow struct {
		Type      string  `json:"type"`
		Name      string  `json:"name"`
		TableName string  `json:"tableName"`
		SQL       *string `json:"sql"`
	}
	rows, err := s.db.Query("SELECT type, name, tbl_name, sql FROM sqlite_master WHERE type IN ('table', 'index') AND (type = 'index' OR name NOT LIKE 'sqlite_%') AND name NOT LIKE 'history_entries_fts_%' ORDER BY type, name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := make([]schemaRow, 0)
	for rows.Next() {
		var r schemaRow
		var st sql.NullString
		if err := rows.Scan(&r.Type, &r.Name, &r.TableName, &st); err != nil {
			t.Fatal(err)
		}
		if st.Valid {
			r.SQL = &st.String
		}
		out = append(out, r)
	}
	b, err := json.MarshalIndent(map[string]any{"schema": out}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("testdata", "sqlite-schema.json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
