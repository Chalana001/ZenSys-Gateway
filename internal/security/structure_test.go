package security

import (
	"os"
	"strings"
	"testing"
)

// This file guards the property the milestone's exit criteria name directly:
// "token comparison is constant-time (crypto/subtle)".
//
// A unit test cannot measure timing reliably enough to be evidence — a
// constant-time comparison and a short-circuiting one differ by nanoseconds,
// and the difference is drowned by scheduler noise on a machine running a race
// detector. What *can* be checked exactly is the shape of the code that
// performs the comparison, which is what this file does.
//
// The checks are structural, so the checkers themselves are tested against
// synthetic sources that should fail. A source-scanning guard that has stopped
// matching reports success, and that is the failure mode worth spending a
// negative control on.

const sourceFile = "serviceauth.go"

func readSource(t *testing.T) string {
	t.Helper()

	src, err := os.ReadFile(sourceFile)
	if err != nil {
		t.Fatalf("read %s: %v", sourceFile, err)
	}
	return string(src)
}

// extractBlock returns the text of the block opened by the first `{` at or
// after the marker, including the marker itself, and reports whether it found
// one.
//
// Brace matching is textual rather than syntactic. That is sufficient here
// because the code being inspected is a single hand-written function whose
// braces are balanced and whose string literals contain no braces — and the
// alternative, parsing the file, would make this guard depend on a parser it
// does not otherwise need.
func extractBlock(src, marker string) (string, bool) {
	start := strings.Index(src, marker)
	if start < 0 {
		return "", false
	}

	open := strings.Index(src[start:], "{")
	if open < 0 {
		return "", false
	}
	open += start

	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1], true
			}
		}
	}
	return "", false
}

// checkIdentifyIsConstantTime inspects the source of tokenAuth.Identify and
// returns one message per violated property.
//
// It returns messages rather than a bool so that a failure names which
// property broke, instead of reporting "the guard failed" and leaving the
// reader to diff the function.
func checkIdentifyIsConstantTime(src string) []string {
	var problems []string

	identify, ok := extractBlock(src, "func (a *tokenAuth) Identify(")
	if !ok {
		return []string{"tokenAuth.Identify was not found; the guard is inspecting nothing"}
	}

	if !strings.Contains(identify, "subtle.ConstantTimeCompare(") {
		problems = append(problems,
			"tokenAuth.Identify does not call subtle.ConstantTimeCompare; a token compared with == is a remote timing oracle")
	}

	loop, ok := extractBlock(identify, "for i := range a.entries")
	if !ok {
		problems = append(problems,
			"tokenAuth.Identify does not iterate over a.entries; every configured token must be compared, or the time taken depends on which one matched")
		return problems
	}

	if strings.Contains(loop, "return ") {
		problems = append(problems,
			"the comparison loop contains a return; leaving the loop on a match makes the response time depend on the position of the matching token")
	}

	// A direct comparison of the presented digest. ConstantTimeCompare is the
	// only sanctioned way to compare two secrets, so any of these is a defect
	// even when the surrounding code looks correct.
	for _, forbidden := range []string{"digest ==", "== digest", "bytes.Equal(", "reflect.DeepEqual("} {
		if strings.Contains(identify, forbidden) {
			problems = append(problems,
				"tokenAuth.Identify compares a digest with "+forbidden+", which is not constant-time")
		}
	}

	return problems
}

// TestSecurity_IdentifyComparesInConstantTime is the guard.
func TestSecurity_IdentifyComparesInConstantTime(t *testing.T) {
	src := readSource(t)

	// Prove the extractors found the real code before trusting a pass. If the
	// function were renamed, both extractions would fail and the checks above
	// would be inspecting an empty string.
	identify, ok := extractBlock(src, "func (a *tokenAuth) Identify(")
	if !ok {
		t.Fatal("could not locate tokenAuth.Identify in " + sourceFile)
	}
	if !strings.Contains(identify, "bearerToken(r)") {
		t.Fatalf("the extracted Identify body does not look like Identify:\n%s", identify)
	}

	if problems := checkIdentifyIsConstantTime(src); len(problems) > 0 {
		for _, p := range problems {
			t.Error(p)
		}
	}
}

// TestSecurity_PackageLevelVarsAreSentinelsOnly narrows the §31 invariant to
// the rule that is specific to this package.
//
// internal/archtest already fails the build on any package-level variable that
// could hold per-request routing state, and it covers this package. What it
// cannot know is that *this* package's package-level variables hold secrets, so
// the narrower rule is asserted here: the only package-level variables allowed
// are error sentinels. A `var currentToken string` would be shared by every
// request and would make authentication order-dependent, and it would look
// unremarkable to a reviewer because the file is full of legitimate
// declarations.
func TestSecurity_PackageLevelVarsAreSentinelsOnly(t *testing.T) {
	for _, file := range []string{"serviceauth.go", "authz.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}

		for _, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)

			// Column 0 means package scope; an indented declaration is inside
			// a function.
			if line != trimmed || !strings.HasPrefix(trimmed, "var ") {
				continue
			}
			if !strings.Contains(trimmed, "errors.New(") {
				t.Errorf("%s: package-level variable that is not an error sentinel: %q", file, trimmed)
			}
		}
	}
}

// TestPackageLevelVarCheckerDetectsMutableState is the negative control: the
// rule above only means something if it rejects the thing it is aimed at.
func TestPackageLevelVarCheckerDetectsMutableState(t *testing.T) {
	src := "package security\n\nvar currentToken string\n\nvar ErrX = errors.New(\"x\")\n"

	var offenders []string
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if line != trimmed || !strings.HasPrefix(trimmed, "var ") {
			continue
		}
		if !strings.Contains(trimmed, "errors.New(") {
			offenders = append(offenders, trimmed)
		}
	}

	if len(offenders) != 1 || !strings.Contains(offenders[0], "currentToken") {
		t.Fatalf("the checker did not isolate the mutable declaration; offenders: %v", offenders)
	}
}

// --- negative controls for the analyser ---

// TestConstantTimeAnalyserDetectsAnEarlyReturn is the control that matters
// most: the shape of the loop is the thing a plausible-looking change breaks.
func TestConstantTimeAnalyserDetectsAnEarlyReturn(t *testing.T) {
	src := `
func (a *tokenAuth) Identify(r *http.Request) (ServiceIdentity, error) {
	presented, ok := bearerToken(r)
	if !ok {
		return ServiceIdentity{}, ErrUnauthenticated
	}
	digest := sha256.Sum256([]byte(presented))
	for i := range a.entries {
		if subtle.ConstantTimeCompare(digest[:], a.entries[i].digest[:]) == 1 {
			return ServiceIdentity{Service: a.entries[i].service}, nil
		}
	}
	return ServiceIdentity{}, ErrUnauthenticated
}
`
	problems := checkIdentifyIsConstantTime(src)
	if !containsSubstring(problems, "return") {
		t.Fatalf("the analyser did not flag an early return; problems: %v", problems)
	}
}

func TestConstantTimeAnalyserDetectsAMissingConstantTimeCall(t *testing.T) {
	src := `
func (a *tokenAuth) Identify(r *http.Request) (ServiceIdentity, error) {
	presented, _ := bearerToken(r)
	digest := sha256.Sum256([]byte(presented))
	idx, found := 0, 0
	for i := range a.entries {
		eq := 0
		if a.entries[i].digest == digest {
			eq = 1
		}
		idx = eq*(i-idx) + idx
		found |= eq
	}
	if found == 0 {
		return ServiceIdentity{}, ErrUnauthenticated
	}
	return ServiceIdentity{Service: a.entries[idx].service}, nil
}
`
	problems := checkIdentifyIsConstantTime(src)
	if !containsSubstring(problems, "ConstantTimeCompare") {
		t.Fatalf("the analyser did not flag a missing constant-time comparison; problems: %v", problems)
	}
}

func TestConstantTimeAnalyserDetectsADirectDigestComparison(t *testing.T) {
	src := `
func (a *tokenAuth) Identify(r *http.Request) (ServiceIdentity, error) {
	presented, _ := bearerToken(r)
	digest := sha256.Sum256([]byte(presented))
	idx, found := 0, 0
	for i := range a.entries {
		eq := 0
		if digest == a.entries[i].digest {
			eq = 1
		}
		idx = eq*(i-idx) + idx
		found |= eq
	}
	if found == 0 {
		return ServiceIdentity{}, ErrUnauthenticated
	}
	return ServiceIdentity{Service: a.entries[idx].service}, nil
}
`
	problems := checkIdentifyIsConstantTime(src)
	if !containsSubstring(problems, "digest ==") && !containsSubstring(problems, "== digest") {
		t.Fatalf("the analyser did not flag a direct digest comparison; problems: %v", problems)
	}
}

func TestConstantTimeAnalyserDetectsAMissingFunction(t *testing.T) {
	problems := checkIdentifyIsConstantTime("package security\n")
	if len(problems) == 0 {
		t.Fatal("the analyser reported success for a source with no Identify function at all")
	}
}

// TestExtractBlockHandlesNesting proves the brace matcher is not confused by a
// nested block, which the real Identify body contains.
func TestExtractBlockHandlesNesting(t *testing.T) {
	src := `
func f() {
	for i := range xs {
		if i > 0 {
			g()
		}
	}
	after()
}
`
	block, ok := extractBlock(src, "for i := range xs")
	if !ok {
		t.Fatal("extractBlock did not find the loop")
	}
	if !strings.Contains(block, "g()") {
		t.Error("the extracted block is missing the nested call")
	}
	if strings.Contains(block, "after()") {
		t.Error("the extracted block ran past its closing brace")
	}
}

func containsSubstring(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
