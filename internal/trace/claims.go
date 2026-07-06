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
// (its doc-comment and body annotations plus its id-named subtests), together
// with any near-miss `// spec:` annotations whose token is not a well-formed
// contract id.
type TestFunc struct {
	Name        string
	File        string
	Line        int
	Claims      []Claim
	BadSpecTags []BadSpecTag
}

// BadSpecTag is a near-miss `// spec:` annotation: the reserved spec: marker
// followed by a token that is not a well-formed contract id (e.g. a trailing
// period, an uppercase slug, or a malformed shape). The gate reports it as a lint
// violation rather than dropping it silently, so a mistyped claim can never leave
// its intended contract in the backlog unnoticed.
type BadSpecTag struct {
	Token string
	File  string
	Line  int
	Func  string
}

// TestFile is a parsed *_test.go file and the test functions it defines.
type TestFile struct {
	Path      string
	TestFuncs []TestFunc
}

// idShape matches a stable contract id (spec section plus lowercase slug),
// mirroring the manifest's own rule. A claim registers only when its token has
// this shape: a `// spec:` annotation, or a subtest name on a *testing.T
// receiver. Ordinary subtest names and prose never register; a `// spec:` marker
// with a non-id token is reported as a near-miss lint violation rather than
// dropped.
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

		// recordComment classifies one comment line: a well-formed `// spec: <id>`
		// annotation becomes a claim, a spec marker with a non-id token becomes a
		// near-miss lint violation, and anything else is ignored.
		recordComment := func(c *ast.Comment) {
			token, hasMarker, idShaped := classifyComment(c.Text)
			if !hasMarker {
				return
			}
			line := fset.Position(c.Pos()).Line
			if idShaped {
				tfn.Claims = append(tfn.Claims, Claim{
					ID: token, Kind: KindAnnotation, File: filename,
					Line: line, Func: fn.Name.Name,
				})
				return
			}
			tfn.BadSpecTags = append(tfn.BadSpecTags, BadSpecTag{
				Token: token, File: filename, Line: line, Func: fn.Name.Name,
			})
		}

		// Annotations on the doc comment, above the function.
		if fn.Doc != nil {
			for _, c := range fn.Doc.List {
				recordComment(c)
			}
		}

		if fn.Body != nil {
			// Annotations sitting inside the function body.
			for _, grp := range file.Comments {
				if grp.Pos() < fn.Body.Lbrace || grp.End() > fn.Body.Rbrace {
					continue
				}
				for _, c := range grp.List {
					recordComment(c)
				}
			}
			// Subtests whose name is a contract id, on a Run call whose receiver
			// resolves through the function's lexical scopes to a *testing.T
			// parameter (never a shadowing local or a sibling closure's param).
			for _, hit := range scanFunc(fn) {
				tfn.Claims = append(tfn.Claims, Claim{
					ID: hit.id, Kind: KindSubtest, File: filename,
					Line: fset.Position(hit.pos).Line, Func: fn.Name.Name,
				})
			}
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

// classifyComment inspects one raw comment line. hasMarker is true when the
// comment is a `// spec:` annotation: its text, after the leading marker slashes
// and surrounding space are stripped, begins with the reserved spec: marker (so a
// `spec:` word mid-sentence never registers). When hasMarker is true, token is
// the first whitespace-delimited word after the marker and idShaped reports
// whether that token is a well-formed contract id. A well-formed annotation is a
// claim; a marker with a non-id token is a near-miss the gate lints rather than
// dropping silently.
func classifyComment(raw string) (token string, hasMarker, idShaped bool) {
	body := strings.TrimLeft(raw, "/*")
	body = strings.TrimSpace(body)
	m := specAnnotation.FindStringSubmatch(body)
	if m == nil {
		return "", false, false
	}
	return m[1], true, idShape.MatchString(m[1])
}

// claimHit is a subtest claim the scanner discovered: the contract id and the
// position of the id string literal, for the enclosing test's Claim list.
type claimHit struct {
	id  string
	pos token.Pos
}

// tScope is one lexical scope: it maps each name bound in the scope to whether
// that binding is a *testing.T parameter -- the only binding a subtest claim may
// resolve to. A local variable, a := alias, a range variable, or an unrelated
// closure's non-T parameter binds the name to false, so it shadows an outer
// *testing.T parameter of the same name.
type tScope map[string]bool

// claimScanner walks a test function tracking lexical scopes, so a
// t.Run("<id>", ...) subtest registers a claim only when its receiver identifier
// resolves, innermost scope first, to a *testing.T parameter in scope. This
// closes the false-claim vectors a flat name set cannot: a same-named local
// (var t = mock{}) shadowing the test's t, and a same-named parameter of an
// unrelated sibling closure whose scope has already closed at the call site.
type claimScanner struct {
	stack []tScope
	hits  []claimHit
}

// scanFunc returns every subtest claim in fn, each resolved against fn's lexical
// scopes.
func scanFunc(fn *ast.FuncDecl) []claimHit {
	cs := &claimScanner{}
	cs.push()
	cs.bindParams(fn.Type)
	if fn.Body != nil {
		cs.walkBlock(fn.Body)
	}
	cs.pop()
	return cs.hits
}

func (cs *claimScanner) push() { cs.stack = append(cs.stack, tScope{}) }
func (cs *claimScanner) pop()  { cs.stack = cs.stack[:len(cs.stack)-1] }

// bind records name in the innermost scope, isTestingT true only for a
// *testing.T parameter. The blank identifier binds nothing.
func (cs *claimScanner) bind(name string, isTestingT bool) {
	if name == "" || name == "_" {
		return
	}
	cs.stack[len(cs.stack)-1][name] = isTestingT
}

// resolve reports whether name resolves, innermost scope first, to a *testing.T
// parameter binding. An unbound name (package-level or undefined) resolves to
// false, so it never claims.
func (cs *claimScanner) resolve(name string) bool {
	for i := len(cs.stack) - 1; i >= 0; i-- {
		if isTestingT, ok := cs.stack[i][name]; ok {
			return isTestingT
		}
	}
	return false
}

// bindParams binds a function or function-literal signature's parameters into the
// innermost scope, marking each *testing.T parameter as such.
func (cs *claimScanner) bindParams(typ *ast.FuncType) {
	if typ == nil || typ.Params == nil {
		return
	}
	for _, f := range typ.Params.List {
		isT := isTestingTPtr(f.Type)
		for _, n := range f.Names {
			cs.bind(n.Name, isT)
		}
	}
}

// walkBlock walks a block in its own scope, visiting statements in source order
// so a binding is in scope for the statements that follow it.
func (cs *claimScanner) walkBlock(b *ast.BlockStmt) {
	cs.push()
	for _, stmt := range b.List {
		cs.walkStmt(stmt)
	}
	cs.pop()
}

// walkStmt visits one statement: it records any bindings it introduces into the
// current scope, opens nested scopes for control-flow bodies, and descends into
// expressions to find Run calls and function literals. Statements that neither
// bind a name nor contain an expression are ignored (nothing to claim).
func (cs *claimScanner) walkStmt(stmt ast.Stmt) {
	switch s := stmt.(type) {
	case *ast.BlockStmt:
		cs.walkBlock(s)
	case *ast.DeclStmt:
		cs.walkDecl(s.Decl)
	case *ast.AssignStmt:
		for _, rhs := range s.Rhs {
			cs.walkExpr(rhs)
		}
		if s.Tok == token.DEFINE {
			for _, lhs := range s.Lhs {
				if id, ok := lhs.(*ast.Ident); ok {
					cs.bind(id.Name, false)
				}
			}
		} else {
			for _, lhs := range s.Lhs {
				cs.walkExpr(lhs)
			}
		}
	case *ast.ExprStmt:
		cs.walkExpr(s.X)
	case *ast.ReturnStmt:
		for _, r := range s.Results {
			cs.walkExpr(r)
		}
	case *ast.GoStmt:
		cs.walkExpr(s.Call)
	case *ast.DeferStmt:
		cs.walkExpr(s.Call)
	case *ast.SendStmt:
		cs.walkExpr(s.Chan)
		cs.walkExpr(s.Value)
	case *ast.IncDecStmt:
		cs.walkExpr(s.X)
	case *ast.LabeledStmt:
		cs.walkStmt(s.Stmt)
	case *ast.IfStmt:
		cs.push()
		cs.walkStmt(s.Init)
		cs.walkExpr(s.Cond)
		cs.walkBlock(s.Body)
		cs.walkStmt(s.Else)
		cs.pop()
	case *ast.ForStmt:
		cs.push()
		cs.walkStmt(s.Init)
		cs.walkExpr(s.Cond)
		cs.walkStmt(s.Post)
		cs.walkBlock(s.Body)
		cs.pop()
	case *ast.RangeStmt:
		cs.push()
		cs.walkExpr(s.X)
		if s.Tok == token.DEFINE {
			cs.bindLHS(s.Key, s.Value)
		}
		cs.walkBlock(s.Body)
		cs.pop()
	case *ast.SwitchStmt:
		cs.push()
		cs.walkStmt(s.Init)
		cs.walkExpr(s.Tag)
		cs.walkBody(s.Body)
		cs.pop()
	case *ast.TypeSwitchStmt:
		cs.push()
		cs.walkStmt(s.Init)
		cs.walkStmt(s.Assign)
		cs.walkBody(s.Body)
		cs.pop()
	case *ast.SelectStmt:
		cs.walkBody(s.Body)
	case *ast.CaseClause:
		cs.push()
		for _, e := range s.List {
			cs.walkExpr(e)
		}
		for _, st := range s.Body {
			cs.walkStmt(st)
		}
		cs.pop()
	case *ast.CommClause:
		cs.push()
		cs.walkStmt(s.Comm)
		for _, st := range s.Body {
			cs.walkStmt(st)
		}
		cs.pop()
	}
}

// walkBody walks the clause list of a switch/select body; each clause opens its
// own scope in walkStmt.
func (cs *claimScanner) walkBody(b *ast.BlockStmt) {
	if b == nil {
		return
	}
	for _, clause := range b.List {
		cs.walkStmt(clause)
	}
}

// bindLHS binds each identifier lvalue as a non-*testing.T local (an alias of a
// *testing.T is deliberately not honored: it stays visibly in the gap list).
func (cs *claimScanner) bindLHS(exprs ...ast.Expr) {
	for _, e := range exprs {
		if id, ok := e.(*ast.Ident); ok {
			cs.bind(id.Name, false)
		}
	}
}

// walkDecl binds the names of a var/const declaration as non-*testing.T locals
// and descends into their values to find nested function literals and Run calls.
func (cs *claimScanner) walkDecl(decl ast.Decl) {
	gen, ok := decl.(*ast.GenDecl)
	if !ok {
		return
	}
	for _, spec := range gen.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for _, v := range vs.Values {
			cs.walkExpr(v)
		}
		for _, n := range vs.Names {
			cs.bind(n.Name, false)
		}
	}
}

// walkExpr descends an expression, recording a claim for every qualifying Run
// call and opening a fresh scope for every function literal it contains.
func (cs *claimScanner) walkExpr(expr ast.Expr) {
	switch e := expr.(type) {
	case *ast.FuncLit:
		cs.push()
		cs.bindParams(e.Type)
		if e.Body != nil {
			cs.walkBlock(e.Body)
		}
		cs.pop()
	case *ast.CallExpr:
		cs.recordRunCall(e)
		cs.walkExpr(e.Fun)
		for _, a := range e.Args {
			cs.walkExpr(a)
		}
	case *ast.ParenExpr:
		cs.walkExpr(e.X)
	case *ast.SelectorExpr:
		cs.walkExpr(e.X)
	case *ast.StarExpr:
		cs.walkExpr(e.X)
	case *ast.UnaryExpr:
		cs.walkExpr(e.X)
	case *ast.BinaryExpr:
		cs.walkExpr(e.X)
		cs.walkExpr(e.Y)
	case *ast.IndexExpr:
		cs.walkExpr(e.X)
		cs.walkExpr(e.Index)
	case *ast.IndexListExpr:
		cs.walkExpr(e.X)
		for _, idx := range e.Indices {
			cs.walkExpr(idx)
		}
	case *ast.SliceExpr:
		cs.walkExpr(e.X)
		cs.walkExpr(e.Low)
		cs.walkExpr(e.High)
		cs.walkExpr(e.Max)
	case *ast.TypeAssertExpr:
		cs.walkExpr(e.X)
	case *ast.KeyValueExpr:
		cs.walkExpr(e.Key)
		cs.walkExpr(e.Value)
	case *ast.CompositeLit:
		for _, el := range e.Elts {
			cs.walkExpr(el)
		}
	}
}

// recordRunCall records a subtest claim when call is a Run on a receiver
// identifier resolving to a *testing.T parameter and its first argument is a
// string literal with contract-id shape.
func (cs *claimScanner) recordRunCall(call *ast.CallExpr) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Run" || len(call.Args) == 0 {
		return
	}
	recv, ok := sel.X.(*ast.Ident)
	if !ok || !cs.resolve(recv.Name) {
		return
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return
	}
	name, err := strconv.Unquote(lit.Value)
	if err != nil || !idShape.MatchString(name) {
		return
	}
	cs.hits = append(cs.hits, claimHit{id: name, pos: lit.Pos()})
}

// isTestingTPtr reports whether expr is a pointer to a T selector type, i.e.
// *testing.T (matched loosely as *<pkg>.T, mirroring go test's own tolerance for
// an aliased import). It marks both a test function's parameter and the
// *testing.T a subtest closure receives.
func isTestingTPtr(expr ast.Expr) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "T"
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
	return isTestingTPtr(fn.Type.Params.List[0].Type)
}
