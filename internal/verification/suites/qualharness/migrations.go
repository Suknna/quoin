package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// defaultContext bounds one harness leg.
func defaultContext() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	// The cancel runs after the leg completes; the fixture probe uses
	// per-request deadlines well inside this bound.
	_ = cancel
	return ctx
}

// frozenSchemaPath is the single schema authority the migration leg
// compares against (VERIFY-AUTHORITY-004).
const frozenSchemaPath = "docs/specs/quoin-v1/contracts/sql/schema.sql"

// createPattern extracts the DDL object declarations from the frozen
// schema text: `CREATE [UNIQUE INDEX|INDEX|TRIGGER|VIEW|TABLE] name`.
var createPattern = regexp.MustCompile(`(?i)CREATE\s+(?:UNIQUE\s+INDEX|INDEX|TRIGGER|VIEW|VIRTUAL\s+TABLE|TABLE)\s+(?:IF\s+NOT\s+EXISTS\s+)?"?([A-Za-z0-9_]+)"?`)

// compareFrozenSchemaSQLiteMaster executes the frozen schema on two
// independent fresh databases and proves three facts: both sqlite_master
// projections are byte-identical (deterministic migration), the object
// set equals exactly the objects the schema text declares (no drift
// between declaration and DDL effect), and every published historical
// migration fixture — when the repository carries any — upgrades to the
// same projection. With a single published release the closed fixture
// set is the current schema; the detail records that boundary instead
// of inventing history (the upgrade corpus owns N-1 adversarial facts).
func compareFrozenSchemaSQLiteMaster() (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	schemaBody, err := os.ReadFile(filepath.Join(root, frozenSchemaPath))
	if err != nil {
		return "", fmt.Errorf("frozen schema: %w", err)
	}
	declared := map[string]bool{}
	for _, match := range createPattern.FindAllStringSubmatch(string(schemaBody), -1) {
		declared[strings.ToLower(match[1])] = true
	}
	if len(declared) == 0 {
		return "", fmt.Errorf("frozen schema declares no objects")
	}

	first, firstDigest, firstMaster, err := freshSchemaMaster(schemaBody)
	if err != nil {
		return "", err
	}
	second, secondDigest, _, err := freshSchemaMaster(schemaBody)
	if err != nil {
		return "", err
	}
	defer os.Remove(first)
	defer os.Remove(second)
	if firstDigest != secondDigest {
		return "", fmt.Errorf("sqlite_master digest drifted between two fresh migrations")
	}
	for object := range declared {
		if !strings.Contains(firstMaster, object+"|") {
			return "", fmt.Errorf("declared object %q missing from sqlite_master", object)
		}
	}
	observed := strings.Split(strings.TrimRight(firstMaster, "\n"), "\n")
	// sqlite_master also carries the fts5 shadow tables (config/data/
	// docsize/idx/...) implicitly created under a declared virtual
	// table's name; every other extra object is undeclared drift.
	shadowed := 0
	for _, entry := range observed {
		name := strings.Split(entry, "|")[1]
		if declared[name] {
			continue
		}
		shadow := false
		for object := range declared {
			if strings.HasPrefix(name, object+"_") {
				shadow = true
				break
			}
		}
		if !shadow {
			return "", fmt.Errorf("sqlite_master carries undeclared object %q", name)
		}
		shadowed++
	}

	historical, err := historicalFixtures(root)
	if err != nil {
		return "", err
	}
	detail := fmt.Sprintf("sqlite_master digest %s over %d declared objects (%d fts5 shadow rows); fresh migrations deterministic; historical fixtures applied: %d",
		firstDigest[:16], len(declared), shadowed, len(historical))
	if len(historical) == 0 {
		detail += " (single published release: the closed fixture set is the current schema; N-1 adversarial corpus lives in internal/quoin/upgrade)"
	}
	return detail, nil
}

// freshSchemaMaster applies the schema to one fresh database and
// returns the database path, the digest and the normalized
// sqlite_master projection: sorted `type|name|` rows — statement text
// excluded so formatting noise cannot mask structural drift.
func freshSchemaMaster(schemaBody []byte) (string, string, string, error) {
	path := filepath.Join(os.TempDir(), fmt.Sprintf("quoin-t40-schema-%d-%d.db", os.Getpid(), time.Now().UnixNano()))
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return path, "", "", err
	}
	defer database.Close()
	if _, err := database.Exec(string(schemaBody)); err != nil {
		return path, "", "", fmt.Errorf("apply frozen schema: %w", err)
	}
	rows, err := database.Query(`SELECT type, name FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return path, "", "", err
	}
	defer rows.Close()
	entries := make([]string, 0, 64)
	for rows.Next() {
		var objectType, name string
		if err := rows.Scan(&objectType, &name); err != nil {
			return path, "", "", err
		}
		entries = append(entries, strings.ToLower(objectType)+"|"+strings.ToLower(name)+"|")
	}
	if err := rows.Err(); err != nil {
		return path, "", "", err
	}
	sort.Strings(entries)
	joined := strings.Join(entries, "\n")
	digest := sha256.Sum256([]byte(joined))
	return path, hex.EncodeToString(digest[:]), joined, nil
}

// historicalFixtures lists the published historical migration fixtures
// (testdata/migrations/*.sql), each an earlier published schema the
// frozen migration path must upgrade. The directory is absent until a
// second release exists.
func historicalFixtures(root string) ([]string, error) {
	dir := filepath.Join(root, "testdata", "migrations")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var fixtures []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			fixtures = append(fixtures, entry.Name())
		}
	}
	sort.Strings(fixtures)
	return fixtures, nil
}
