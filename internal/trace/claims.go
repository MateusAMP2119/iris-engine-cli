package trace

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// ClaimKind distinguishes the two syntaxes a test uses to claim a contract.
type ClaimKind int

// The two recognized claim syntaxes.
const (
	// KindAnnotation is a `// spec: <id>` comment on a test's doc comment or in
	// its body.
	KindAnnotation ClaimKind = iota
	// KindSubtest is a subtest whose name is a contract id, t.Run("<id>", ...).
	KindSubtest
)

// String names the claim syntax, for reports and test diagnostics.
func (k ClaimKind) String() string {
	switch k {
	case KindAnnotation:
		return "annotation"
	case KindSubtest:
		return "subtest"
	default:
		return "unknown"
	}
}

// Claim is one contract claim made by a test: the claimed id, the syntax used,
// and where it sits in the tree.
type Claim struct {
	ID   string
	Kind ClaimKind
	File string
	Line int
	Func string // enclosing Test function name
}

// TestFunc is a single Go test function and the contract claims found within it
// (its doc-comment and body annotations plus its id-named subtests).
type TestFunc struct {
	Name   string
	File   string
	Line   int
	Claims []Claim
}

// TestFile is a parsed *_test.go file and the test functions it defines.
type TestFile struct {
	Path      string
	TestFuncs []TestFunc
}

// idShape matches a stable contract id (spec section plus lowercase slug),
// mirroring the manifest's own rule. It is the sole discriminator for a claim:
// a `// spec:` token or a subtest name counts only when it has this shape, so
// ordinary subtest names and prose never register as claims.
var idShape = regexp.MustCompile(`^S\d\d(\.\d+)?/[a-z0-9-]+$`)

// specAnnotation matches the annotation body of a comment line, after its
// marker and surrounding space have been stripped: `spec: <token>`.
var specAnnotation = regexp.MustCompile(`^spec:\s*(\S+)`)

// ParseTestFile parses one Go test file's source and returns its test functions
// and the claims each makes. It is pure: it reads nothing, so both the id-named
// subtests and the `// spec:` annotations of the source govern the result, and a
// claim written inside a string literal (as test fixtures are) never registers.
func ParseTestFile(filename string, src []byte) (*TestFile, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("trace: parse %s: %w", filename, err)
	}

	tf := &TestFile{Path: filename}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !isTestFunc(fn) {
			continue
		}
		tfn := TestFunc{
			Name: fn.Name.Name,
			File: filename,
			Line: fset.Position(fn.Pos()).Line,
		}

		// Annotations on the doc comment, above the function.
		if fn.Doc != nil {
			for _, c := range fn.Doc.List {
				if id, ok := annotationID(c.Text); ok {
					tfn.Claims = append(tfn.Claims, Claim{
						ID: id, Kind: KindAnnotation, File: filename,
						Line: fset.Position(c.Pos()).Line, Func: fn.Name.Name,
					})
				}
			}
		}

		if fn.Body != nil {
			// Annotations sitting inside the function body.
			for _, grp := range file.Comments {
				if grp.Pos() < fn.Body.Lbrace || grp.End() > fn.Body.Rbrace {
					continue
				}
				for _, c := range grp.List {
					if id, ok := annotationID(c.Text); ok {
						tfn.Claims = append(tfn.Claims, Claim{
							ID: id, Kind: KindAnnotation, File: filename,
							Line: fset.Position(c.Pos()).Line, Func: fn.Name.Name,
						})
					}
				}
			}
			// Subtests whose name is a contract id.
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if id, pos, ok := subtestID(n); ok {
					tfn.Claims = append(tfn.Claims, Claim{
						ID: id, Kind: KindSubtest, File: filename,
						Line: fset.Position(pos).Line, Func: fn.Name.Name,
					})
				}
				return true
			})
		}

		tf.TestFuncs = append(tf.TestFuncs, tfn)
	}
	return tf, nil
}

// ParseTestDir walks root for *_test.go files and parses each, skipping hidden
// directories, vendored code, and testdata trees. Reading repo files is the only
// I/O the gate performs.
func ParseTestDir(root string) ([]*TestFile, error) {
	var out []*TestFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		tf, err := ParseTestFile(path, src)
		if err != nil {
			return err
		}
		out = append(out, tf)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("trace: walk %s: %w", root, err)
	}
	return out, nil
}

// ClaimedIDs folds every claim across the given files into the set of contract
// ids the suite claims, the input the manifest->tests gap direction consumes.
func ClaimedIDs(files []*TestFile) map[string]bool {
	claimed := make(map[string]bool)
	for _, tf := range files {
		for _, fn := range tf.TestFuncs {
			for _, c := range fn.Claims {
				claimed[c.ID] = true
			}
		}
	}
	return claimed
}

// annotationID extracts a `// spec: <id>` claim from a raw comment line's text.
// It reports ok only when the stripped line begins with the spec: marker and the
// token has contract-id shape, so a `spec:` word mid-sentence never registers.
func annotationID(raw string) (string, bool) {
	body := strings.TrimLeft(raw, "/*")
	body = strings.TrimSpace(body)
	m := specAnnotation.FindStringSubmatch(body)
	if m == nil {
		return "", false
	}
	token := m[1]
	if !idShape.MatchString(token) {
		return "", false
	}
	return token, true
}

// subtestID reports the contract id of a t.Run("<id>", ...) call when its first
// argument is a string literal with contract-id shape.
func subtestID(n ast.Node) (string, token.Pos, bool) {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return "", 0, false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Run" || len(call.Args) == 0 {
		return "", 0, false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", 0, false
	}
	name, err := strconv.Unquote(lit.Value)
	if err != nil || !idShape.MatchString(name) {
		return "", 0, false
	}
	return name, lit.Pos(), true
}

// isTestFunc reports whether fn is a Go test function: a free function named
// Test or TestXxx (with a non-lowercase rune after Test, matching go test's own
// rule) taking a single *testing.T parameter.
func isTestFunc(fn *ast.FuncDecl) bool {
	if fn.Recv != nil || fn.Body == nil {
		return false
	}
	name := fn.Name.Name
	if !strings.HasPrefix(name, "Test") {
		return false
	}
	if rest := name[len("Test"):]; rest != "" {
		if r := []rune(rest)[0]; unicode.IsLower(r) {
			return false
		}
	}
	if fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
		return false
	}
	star, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "T"
}
