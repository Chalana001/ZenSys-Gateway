// Package archtest holds tests that assert properties of the source tree
// rather than of a single function.
//
// These are regression guards. Each one passes trivially today and exists to
// fail the day someone reintroduces the pattern it forbids.
package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// requestScopedNames matches package-level identifiers that would indicate
// request-scoped state promoted to process-global mutable state.
//
// The architecture document names the exact anti-pattern (§31):
//
//	global currentTenant = "mangala"
//
// One Gateway process serves every tenant, so a package-level variable holding
// "the tenant for this process" is not merely inelegant — it is a cross-tenant
// data leak waiting for two concurrent requests to overlap.
var requestScopedNames = regexp.MustCompile(`(?i)^_?(current|active|last|global|request)?_?(tenant|route|product|host|slug)s?$`)

// skipDirs are not part of the Gateway's own source.
var skipDirs = map[string]bool{
	".git":          true,
	".workbuddy-ai": true,
	"bin":           true,
	"docs":          true,
	"vendor":        true,
}

// offender is one package-level variable that could hold request state.
type offender struct {
	File string
	Line int
	Name string
	Type string
}

func (o offender) String() string {
	typeName := o.Type
	if typeName == "" {
		typeName = "(inferred)"
	}
	return fmt.Sprintf("%s:%d: var %s %s", o.File, o.Line, o.Name, typeName)
}

// TestNoPackageLevelRequestState scans the Gateway's own packages for a
// package-level variable that could hold per-request routing state.
//
// Constants are exempt: a package-level const cannot be reassigned, so it
// cannot be the thing two concurrent requests fight over. Error sentinels are
// exempt for the same reason — they are declared `var` only because errors.New
// is not a constant expression, and they are never reassigned.
func TestNoPackageLevelRequestState(t *testing.T) {
	root := repoRoot(t)

	var offenders []offender

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if skipDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}

		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}

		// A parse failure means the file is not valid Go, which the compiler
		// reports far more clearly than this test could.
		found, scanErr := scanSource(string(src), filepath.ToSlash(rel))
		if scanErr != nil {
			return nil
		}
		offenders = append(offenders, found...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk source tree: %v", err)
	}

	for _, o := range offenders {
		t.Errorf("package-level request state: %s", o)
	}
	if len(offenders) > 0 {
		t.Errorf("%d package-level variable(s) could hold per-request routing state; "+
			"tenant identity must travel in the request context (§31)", len(offenders))
	}
}

// TestScanForRequestStateFindsTheAntiPattern proves the detector above actually
// bites.
//
// Without this, TestNoPackageLevelRequestState would keep passing if a typo
// made the pattern match nothing at all, and the guard would be silently dead.
func TestScanForRequestStateFindsTheAntiPattern(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want bool // true when the source must be flagged
	}{
		{
			name: "the documented anti-pattern",
			src:  "package p\n\nvar currentTenant = \"mangala\"\n",
			want: true,
		},
		{
			name: "declared with an explicit type",
			src:  "package p\n\ntype Route struct{}\n\nvar currentRoute Route\n",
			want: true,
		},
		{
			name: "a bare tenant variable",
			src:  "package p\n\nvar tenant string\n",
			want: true,
		},
		{
			name: "a package-level host",
			src:  "package p\n\nvar activeHost string\n",
			want: true,
		},
		{
			name: "a plural form",
			src:  "package p\n\nvar currentTenants []string\n",
			want: true,
		},
		{
			name: "error sentinels are fine",
			src:  "package p\n\nimport \"errors\"\n\nvar ErrNotFound = errors.New(\"not found\")\n",
			want: false,
		},
		{
			name: "constants cannot be reassigned",
			src:  "package p\n\nconst currentTenant = \"mangala\"\n",
			want: false,
		},
		{
			name: "a typed constant is still a constant",
			src:  "package p\n\ntype Status string\n\nconst activeStatus Status = \"ACTIVE\"\n",
			want: false,
		},
		{
			name: "a reserved-host list is not request state",
			src:  "package p\n\nvar reservedHosts = []string{\"admin\"}\n",
			want: false,
		},
		{
			name: "an injected clock is not request state",
			src:  "package p\n\nimport \"time\"\n\nvar now = time.Now\n",
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			found, err := scanSource(tc.src, "synthetic.go")
			if err != nil {
				t.Fatalf("scan: %v", err)
			}

			if tc.want && len(found) == 0 {
				t.Error("the detector missed a package-level request-state variable")
			}
			if !tc.want && len(found) > 0 {
				t.Errorf("the detector produced a false positive: %v", found)
			}
		})
	}
}

// scanSource reports package-level variables that look like request state.
func scanSource(src, filename string) ([]offender, error) {
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}

	var found []offender

	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.VAR {
			continue
		}

		for _, spec := range genDecl.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}

			typeName := renderType(valueSpec.Type)

			// An error sentinel is declared `var` only because errors.New is
			// not a constant expression. It is never reassigned, so it cannot
			// carry per-request state.
			if typeName == "error" {
				continue
			}

			for _, name := range valueSpec.Names {
				if !requestScopedNames.MatchString(name.Name) {
					continue
				}
				found = append(found, offender{
					File: filename,
					Line: fset.Position(name.Pos()).Line,
					Name: name.Name,
					Type: typeName,
				})
			}
		}
	}

	return found, nil
}

// renderType renders a type expression as source text, or "" when the type is
// inferred from the initialiser.
func renderType(expr ast.Expr) string {
	if expr == nil {
		return ""
	}

	var sb strings.Builder
	if err := printer.Fprint(&sb, token.NewFileSet(), expr); err != nil {
		return ""
	}
	return sb.String()
}

// repoRoot walks up from the test's working directory to the module root.
//
// `go test` runs each package's tests with the working directory set to the
// package directory, so the module root is an ancestor rather than a fixed
// number of levels up.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}
