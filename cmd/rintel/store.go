package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// The whole DB is a regenerable cache: a schema change bumps this and forces a rescan, no migrations.
const schemaVersion = 3

// damaged reports whether an error says the index file itself is broken (truncated, overwritten, not SQLite).
func damaged(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "malformed") || strings.Contains(err.Error(), "not a database"))
}

const schema = `
CREATE TABLE scans (id INTEGER PRIMARY KEY, parent_scan_id INTEGER, commit_sha TEXT, branch TEXT, dirty INTEGER,
  adapter_version TEXT, started_at TEXT, finished_at TEXT, stats_json TEXT);
CREATE TABLE files (scan_id INTEGER, path TEXT, content_hash TEXT, language TEXT, module TEXT, mtime INTEGER, size INTEGER,
  metrics TEXT, PRIMARY KEY (scan_id, path));
CREATE TABLE modules (scan_id INTEGER, name TEXT, root_path TEXT, build_tool TEXT, PRIMARY KEY (scan_id, name));
CREATE TABLE symbols (scan_id INTEGER, id TEXT, type TEXT, name TEXT, fqn TEXT, parent TEXT, module TEXT, file TEXT,
  l1 INTEGER, c1 INTEGER, l2 INTEGER, c2 INTEGER, external INTEGER NOT NULL DEFAULT 0, attrs TEXT,
  PRIMARY KEY (scan_id, id));
CREATE TABLE dependencies (scan_id INTEGER, src TEXT NOT NULL, dst TEXT NOT NULL, via TEXT NOT NULL DEFAULT '', relation TEXT NOT NULL,
  evidence_level TEXT NOT NULL CHECK (evidence_level IN ('declared','resolved','heuristic')),
  rule TEXT, file TEXT, line INTEGER, origin TEXT NOT NULL,
  CHECK (evidence_level <> 'heuristic' OR rule IS NOT NULL));
CREATE INDEX dep_src ON dependencies (scan_id, src);
CREATE INDEX dep_dst ON dependencies (scan_id, dst);
CREATE TABLE unresolved_refs (scan_id INTEGER, src TEXT, text TEXT, reason TEXT, file TEXT, line INTEGER);
CREATE TABLE api_endpoints (scan_id INTEGER, method TEXT, path TEXT, handler TEXT,
  evidence_level TEXT NOT NULL CHECK (evidence_level IN ('declared','resolved','heuristic')), file TEXT, line INTEGER);
`

func openDB(root string) (*sql.DB, error) {
	dir := filepath.Join(root, ".ri", "cache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// The cache must never be committed by accident (`git add -A` would take a multi-megabyte index along), so the
	// directory ignores itself; nobody has to remember to edit the repository's .gitignore.
	if ignore := filepath.Join(root, ".ri", ".gitignore"); !fileExists(ignore) {
		if err := os.WriteFile(ignore, []byte("# written by rintel: the cache is regenerable and machine-specific\ncache/\n"), 0o644); err != nil {
			return nil, err
		}
	}
	path := filepath.Join(dir, "index.db")
	for attempt := 0; ; attempt++ {
		db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
		if err != nil {
			return nil, err
		}
		var v int
		if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
			if damaged(err) && attempt == 0 { // the index is a cache: a broken one is thrown away and built again
				v = -1
			} else {
				return nil, err
			}
		}
		if v == schemaVersion {
			return db, nil
		}
		if v == 0 {
			if _, err := db.Exec(schema + fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
				return nil, err
			}
			return db, nil
		}
		db.Close()
		if attempt > 0 {
			return nil, fmt.Errorf("cannot reset %s", path)
		}
		for _, f := range []string{path, path + "-wal", path + "-shm"} {
			os.Remove(f)
		}
	}
}

// latestScan returns 0 when the repository has never been scanned.
func latestScan(db *sql.DB) (id int64) {
	db.QueryRow("SELECT COALESCE(MAX(id),0) FROM scans").Scan(&id)
	return
}
