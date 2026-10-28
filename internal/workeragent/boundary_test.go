package workeragent

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const module = "samuori.invalid/samuori/"

// The worker binary must not be able to reach the controller's database or user-facing services (ADR 0024).
func TestWorkerHasNoDatabaseAccess(t *testing.T) {
	t.Run("SEC-WRK-03", func(t *testing.T) {
		root, _ := filepath.Abs("../..")
		forbidden := []string{module + "internal/db", module + "internal/tenancy", module + "internal/auth", module + "internal/api", module + "migrations", "modernc.org/sqlite", "github.com/jackc/pgx", "database/sql"}
		seen := map[string]bool{}
		var visit func(pkg string)
		visit = func(pkg string) {
			if seen[pkg] {
				return
			}
			seen[pkg] = true
			dir := filepath.Join(root, strings.TrimPrefix(pkg, module))
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
					continue
				}
				f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, parser.ImportsOnly)
				if err != nil {
					t.Fatal(err)
				}
				for _, imp := range f.Imports {
					p, _ := strconv.Unquote(imp.Path.Value)
					for _, bad := range forbidden {
						if p == bad || strings.HasPrefix(p, bad+"/") {
							t.Errorf("%s imports %s", pkg, p)
						}
					}
					if strings.HasPrefix(p, module) {
						visit(p)
					}
				}
			}
		}
		visit(module + "cmd/samuori-worker")
	})
}
