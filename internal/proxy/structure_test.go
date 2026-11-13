package proxy

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The structural half of §48.
//
// The behavioural test (TestProxy_ProductOutageIsIsolated) shows that a POS
// outage does not reach GMS today. That is evidence about one code path. This
// file is evidence about the *shape* of the package: it fails if a place is
// ever added where a request for one product could be served by another
// product's upstream.
//
// The two rules below are the ones the doc comment on the route type claims:
// Proxy reaches its upstreams through a single exact-key map, and a route
// cannot reach another route. Both are necessary. A second *route field on
// Proxy is a fallback waiting to be written; a *route field on route makes the
// error handler for one product able to name another product's upstream, which
// is the fallback itself.

// routeContainerField is the single field on Proxy allowed to reference a
// route.
const routeContainerField = "routes"

// routeContainerType is its required type: the exact (product, stage) key, not
// something that would have to be searched.
const routeContainerType = "map[key]*route"

// parseStructs returns every struct type declared in src, as a map from struct
// name to field name to rendered field type.
//
// The type is taken from the source text rather than rendered from the AST so
// that the guard's messages quote what the author wrote, and so that it does
// not depend on go/printer's formatting decisions.
func parseStructs(src []byte) (map[string]map[string]string, error) {
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "source.go", src, 0)
	if err != nil {
		return nil, err
	}

	structs := map[string]map[string]string{}

	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		st, ok := spec.Type.(*ast.StructType)
		if !ok {
			return true
		}

		fields := map[string]string{}
		for _, f := range st.Fields.List {
			rendered := strings.TrimSpace(string(src[f.Type.Pos()-1 : f.Type.End()-1]))
			for _, name := range f.Names {
				fields[name.Name] = rendered
			}
		}
		structs[spec.Name.Name] = fields

		return true
	})

	return structs, nil
}

// checkNoFallbackPath applies the §48 structural rules to a package's struct
// definitions and returns every violation found.
//
// It takes already-parsed structs rather than a file so that it can be run
// across a whole package, and so that its own tests can hand it a synthetic
// shape without going near the filesystem.
func checkNoFallbackPath(structs map[string]map[string]string) []string {
	var problems []string

	// Rule 1: Proxy holds its routes in exactly one field, keyed by the whole
	// route key.
	proxyFields, ok := structs["Proxy"]
	switch {
	case !ok:
		problems = append(problems,
			"the Proxy type is gone; this guard is no longer checking what it was written to check")

	default:
		var holders []string
		for name, typ := range proxyFields {
			if strings.Contains(typ, "*route") {
				holders = append(holders, name+": "+typ)
			}
		}
		sort.Strings(holders)

		switch {
		case len(holders) == 0:
			problems = append(problems,
				"Proxy no longer holds any routes; this guard is no longer checking what it was written to check")

		case len(holders) > 1:
			problems = append(problems, fmt.Sprintf(
				"Proxy holds routes in more than one field (%s); more than one place to look a route up is more than one place to fall back from",
				strings.Join(holders, ", ")))

		default:
			if want := routeContainerField + ": " + routeContainerType; holders[0] != want {
				problems = append(problems, fmt.Sprintf(
					"Proxy's route container is %q, want %q: the lookup must be the exact (product, stage) key, not a search over candidates",
					holders[0], want))
			}
		}
	}

	// Rule 2: a route cannot reach another route.
	routeFields, ok := structs["route"]
	switch {
	case !ok:
		problems = append(problems,
			"the route type is gone; this guard is no longer checking what it was written to check")

	default:
		for name, typ := range routeFields {
			reaches := strings.Contains(typ, "*route") ||
				strings.Contains(typ, "[]route") ||
				(strings.Contains(typ, "map[") && strings.Contains(typ, "route"))

			if reaches {
				problems = append(problems, fmt.Sprintf(
					"the route type has a field %q of type %q, so one route can reach another; a fallback between products becomes expressible",
					name, typ))
			}
		}
	}

	sort.Strings(problems)
	return problems
}

// TestProxy_HasNoCrossProductFallbackPath is §48 asserted structurally, over
// the package's real source.
func TestProxy_HasNoCrossProductFallbackPath(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list the package: %v", err)
	}

	structs := map[string]map[string]string{}
	var parsed []string

	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}

		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if len(src) == 0 {
			t.Fatalf("%s is empty; the guard would pass vacuously", name)
		}

		found, err := parseStructs(src)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for typeName, fields := range found {
			if _, dup := structs[typeName]; dup {
				t.Fatalf("the type %s is declared in two files; the guard's view of the package is ambiguous", typeName)
			}
			structs[typeName] = fields
		}
		parsed = append(parsed, name)
	}

	if len(parsed) == 0 {
		t.Fatal("no non-test source files were parsed; the guard would pass vacuously")
	}

	for _, problem := range checkNoFallbackPath(structs) {
		t.Errorf("%s", problem)
	}
}

// TestFallbackAnalyzerDetectsAFallback is the control for the guard above.
//
// A source-scanning guard is a guard that can be silently disabled: rename a
// type, change a field's spelling, and it reports nothing while looking like it
// passed. Each case here is a shape the guard is supposed to catch, so if the
// rules ever stop matching, this test fails instead of the guard quietly
// approving everything.
func TestFallbackAnalyzerDetectsAFallback(t *testing.T) {
	const (
		goodSource = `
package proxy

type key struct {
	product string
	stage   string
}

type route struct {
	product string
	stage   string
	target  string
}

type Proxy struct {
	logger       string
	routes       map[key]*route
	preserveHost bool
}
`

		secondRouteFieldSource = `
package proxy

type key struct{ product string }

type route struct{ product string }

type Proxy struct {
	routes   map[key]*route
	fallback *route
}
`

		routeSliceSource = `
package proxy

type key struct{ product string }

type route struct{ product string }

type Proxy struct {
	routes  map[key]*route
	ordered []*route
}
`

		chainedRouteSource = `
package proxy

type key struct{ product string }

type route struct {
	product string
	next    *route
}

type Proxy struct {
	routes map[key]*route
}
`

		routeMapSource = `
package proxy

type key struct{ product string }

type route struct {
	product  string
	siblings map[string]route
}

type Proxy struct {
	routes map[key]*route
}
`

		renamedProxySource = `
package proxy

type key struct{ product string }

type route struct{ product string }

type Proxies struct {
	routes map[key]*route
}
`

		searchedLookupSource = `
package proxy

type key struct{ product string }

type route struct{ product string }

type Proxy struct {
	routes map[string]*route
}
`
	)

	tests := []struct {
		name     string
		source   string
		wantNone bool
		want     []string // substrings, any of which satisfies the expectation
	}{
		{
			name:     "the real shape is accepted",
			source:   goodSource,
			wantNone: true,
		},
		{
			name:   "a second route field on Proxy",
			source: secondRouteFieldSource,
			want:   []string{"more than one field", "fallback"},
		},
		{
			name:   "a slice of routes to scan in order",
			source: routeSliceSource,
			want:   []string{"more than one field", "ordered"},
		},
		{
			name:   "a route that points at another route",
			source: chainedRouteSource,
			want:   []string{"can reach another"},
		},
		{
			name:   "a route holding a map of routes",
			source: routeMapSource,
			want:   []string{"can reach another"},
		},
		{
			name:   "the guarded types renamed away",
			source: renamedProxySource,
			want:   []string{"no longer checking"},
		},
		{
			name:   "a lookup key that is not the exact route key",
			source: searchedLookupSource,
			want:   []string{"route container", "exact (product, stage) key"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			structs, err := parseStructs([]byte(tc.source))
			if err != nil {
				t.Fatalf("parse the fixture: %v", err)
			}

			problems := checkNoFallbackPath(structs)

			if tc.wantNone {
				if len(problems) > 0 {
					t.Fatalf("the guard rejected a shape it is supposed to accept:\n%s",
						strings.Join(problems, "\n"))
				}
				return
			}

			if len(problems) == 0 {
				t.Fatal("the guard found nothing to complain about; it no longer detects this shape")
			}

			joined := strings.Join(problems, "\n")
			for _, want := range tc.want {
				if strings.Contains(joined, want) {
					return
				}
			}
			t.Errorf("the guard complained, but not about %v:\n%s", tc.want, joined)
		})
	}
}

// TestParseStructsReadsEveryField guards the guard's own input parsing.
//
// If parseStructs silently returned nothing — an AST change, a mistaken node
// type — every check above would find no violations and report success. This
// asserts that the shapes it is meant to see actually arrive.
func TestParseStructsReadsEveryField(t *testing.T) {
	const src = `
package proxy

type key struct {
	product string
	stage   string
}

type route struct {
	product string
	target  *url.URL
}

type Proxy struct {
	logger       *slog.Logger
	routes       map[key]*route
	preserveHost bool
}
`

	structs, err := parseStructs([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if got := len(structs); got != 3 {
		t.Fatalf("parsed %d structs, want 3: %v", got, structs)
	}

	proxy := structs["Proxy"]
	if got := len(proxy); got != 3 {
		t.Fatalf("Proxy has %d fields, want 3: %v", got, proxy)
	}
	if got := proxy["routes"]; got != "map[key]*route" {
		t.Errorf("Proxy.routes is %q, want %q", got, "map[key]*route")
	}
	if got := proxy["preserveHost"]; got != "bool" {
		t.Errorf("Proxy.preserveHost is %q, want %q", got, "bool")
	}
	if got := structs["route"]["target"]; got != "*url.URL" {
		t.Errorf("route.target is %q, want %q", got, "*url.URL")
	}
}
