// Package judge is the benchmark harness for the Base −2 Calculator problem
// (bench/PROBLEM.md): a reference oracle (tokenizer + recursive-descent parser
// over big.Int, floor division, negabinary conversion), a deterministic test
// case generator, and a runner that compiles a candidate single-file Go program
// and scores it against the hidden cases.
//
// The oracle defines ground truth for the whole benchmark, so it is itself
// validated by brute-force self-tests in oracle_test.go before anything is
// judged against it.
package judge

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ---------- Negabinary conversion ----------

// FromNegabinary parses a base −2 digit string into its integer value (Horner).
func FromNegabinary(s string) (*big.Int, error) {
	if s == "" {
		return nil, fmt.Errorf("empty literal")
	}
	v := new(big.Int)
	base := big.NewInt(-2)
	one := big.NewInt(1)
	for _, c := range s {
		switch c {
		case '0':
			v.Mul(v, base)
		case '1':
			v.Mul(v, base)
			v.Add(v, one)
		default:
			return nil, fmt.Errorf("invalid negabinary digit %q", c)
		}
	}
	return v, nil
}

// ToNegabinary renders v in base −2: digits 0/1, no leading zeros, "0" for zero.
func ToNegabinary(v *big.Int) string {
	if v.Sign() == 0 {
		return "0"
	}
	n := new(big.Int).Set(v)
	two := big.NewInt(2)
	negTwo := big.NewInt(-2)
	r := new(big.Int)
	var digits []byte
	for n.Sign() != 0 {
		// Euclidean mod 2 → r ∈ {0,1}; (n − r) is then exactly divisible by −2.
		r.Mod(n, two)
		digits = append(digits, byte('0'+r.Int64()))
		n.Sub(n, r)
		n.Quo(n, negTwo)
	}
	// digits were produced least-significant first.
	for i, j := 0, len(digits)-1; i < j; i, j = i+1, j-1 {
		digits[i], digits[j] = digits[j], digits[i]
	}
	return string(digits)
}

// floorDiv returns ⌊a/b⌋ (rounded toward −∞). b must be nonzero.
func floorDiv(a, b *big.Int) *big.Int {
	q := new(big.Int)
	r := new(big.Int)
	q.QuoRem(a, b, r) // truncated toward zero; r has the sign of a
	if r.Sign() != 0 && (a.Sign() < 0) != (b.Sign() < 0) {
		q.Sub(q, big.NewInt(1))
	}
	return q
}

// ---------- Oracle: tokenizer + recursive-descent evaluator ----------

type tokKind int

const (
	tokNum tokKind = iota
	tokOp          // + - * /
	tokLParen
	tokRParen
)

type token struct {
	kind tokKind
	text string // literal digits for tokNum, operator char for tokOp
}

func tokenize(input string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(input) {
		c := input[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case c == '0' || c == '1':
			j := i
			for j < len(input) && (input[j] == '0' || input[j] == '1') {
				j++
			}
			toks = append(toks, token{tokNum, input[i:j]})
			i = j
		case c == '+' || c == '-' || c == '*' || c == '/':
			toks = append(toks, token{tokOp, string(c)})
			i++
		case c == '(':
			toks = append(toks, token{tokLParen, "("})
			i++
		case c == ')':
			toks = append(toks, token{tokRParen, ")"})
			i++
		default:
			return nil, fmt.Errorf("unexpected character %q at offset %d", c, i)
		}
	}
	return toks, nil
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() (token, bool) {
	if p.pos < len(p.toks) {
		return p.toks[p.pos], true
	}
	return token{}, false
}

// expr := term (('+'|'-') term)*
func (p *parser) expr() (*big.Int, error) {
	v, err := p.term()
	if err != nil {
		return nil, err
	}
	for {
		t, ok := p.peek()
		if !ok || t.kind != tokOp || (t.text != "+" && t.text != "-") {
			return v, nil
		}
		p.pos++
		rhs, err := p.term()
		if err != nil {
			return nil, err
		}
		if t.text == "+" {
			v = new(big.Int).Add(v, rhs)
		} else {
			v = new(big.Int).Sub(v, rhs)
		}
	}
}

// term := unary (('*'|'/') unary)*
func (p *parser) term() (*big.Int, error) {
	v, err := p.unary()
	if err != nil {
		return nil, err
	}
	for {
		t, ok := p.peek()
		if !ok || t.kind != tokOp || (t.text != "*" && t.text != "/") {
			return v, nil
		}
		p.pos++
		rhs, err := p.unary()
		if err != nil {
			return nil, err
		}
		if t.text == "*" {
			v = new(big.Int).Mul(v, rhs)
		} else {
			if rhs.Sign() == 0 {
				return nil, fmt.Errorf("division by zero")
			}
			v = floorDiv(v, rhs)
		}
	}
}

// unary := '-' unary | primary
func (p *parser) unary() (*big.Int, error) {
	if t, ok := p.peek(); ok && t.kind == tokOp && t.text == "-" {
		p.pos++
		v, err := p.unary()
		if err != nil {
			return nil, err
		}
		return new(big.Int).Neg(v), nil
	}
	return p.primary()
}

// primary := NUM | '(' expr ')'
func (p *parser) primary() (*big.Int, error) {
	t, ok := p.peek()
	if !ok {
		return nil, fmt.Errorf("unexpected end of expression")
	}
	switch t.kind {
	case tokNum:
		p.pos++
		return FromNegabinary(t.text)
	case tokLParen:
		p.pos++
		v, err := p.expr()
		if err != nil {
			return nil, err
		}
		nt, ok := p.peek()
		if !ok || nt.kind != tokRParen {
			return nil, fmt.Errorf("missing closing parenthesis")
		}
		p.pos++
		return v, nil
	default:
		return nil, fmt.Errorf("unexpected token %q", t.text)
	}
}

// EvalExpr is the oracle: it evaluates a Base −2 Calculator expression and
// returns the result in negabinary.
func EvalExpr(input string) (string, error) {
	toks, err := tokenize(input)
	if err != nil {
		return "", err
	}
	p := &parser{toks: toks}
	v, err := p.expr()
	if err != nil {
		return "", err
	}
	if p.pos != len(p.toks) {
		return "", fmt.Errorf("trailing tokens at position %d", p.pos)
	}
	return ToNegabinary(v), nil
}

// ---------- Test cases ----------

// Case is one hidden test: an input expression. The expected output is always
// computed by the oracle, never stored, so cases and oracle cannot drift apart.
type Case struct {
	Input string
}

// nb renders a small decimal value in negabinary (handcrafted-case helper).
func nb(n int64) string { return ToNegabinary(big.NewInt(n)) }

// Cases returns the full deterministic hidden test set: handcrafted edge cases
// first, then seeded random expressions (all validated division-by-zero-free).
func Cases() []Case {
	cases := []Case{
		{"0"},
		{"1"},
		{"11"},                               // −1
		{"110"},                              // 2
		{"-1"},                               // −1 → "11"
		{"--1"},                              // 1
		{"---110"},                           // −2 → "10"
		{"1+1"},                              // 2
		{"1-11"},                             // 1 − (−1) = 2
		{"( 1 + 11 ) * 110"},                 // 0
		{"  1\t+\t1  "},                      // whitespace variants
		{nb(7) + "/" + nb(2)},                // 3
		{nb(-7) + "/" + nb(2)},               // −4 (floor, not trunc)
		{nb(7) + "/" + nb(-2)},               // −4
		{nb(-7) + "/" + nb(-2)},              // 3
		{"-" + nb(6) + "/" + nb(4)},          // (−6)/4 = −2 (unary binds tighter)
		{nb(-8) + "/" + nb(3) + "*" + nb(3)}, // floor(−8/3)·3 = −9
		{"(" + nb(100) + "-" + nb(100) + ")/" + nb(37)}, // 0/37 = 0
		{nb(1000000007) + "*" + nb(998244353)},
		{"-(" + nb(5) + "-" + nb(9) + ")"}, // 4
		{"((((" + nb(42) + "))))"},
		{"1" + strings.Repeat("0", 199)}, // 200-digit literal identity
	}
	r := rand.New(rand.NewSource(20260729))
	for len(cases) < 172 {
		e := genExpr(r, 5)
		if _, err := EvalExpr(e); err != nil {
			continue // division by zero somewhere — discard and regenerate
		}
		if len(e) > 20000 {
			continue
		}
		cases = append(cases, Case{e})
	}
	return cases
}

// genLiteral produces a random valid negabinary literal.
func genLiteral(r *rand.Rand) string {
	var length int
	switch p := r.Intn(10); {
	case p < 7:
		length = 1 + r.Intn(12)
	case p < 9:
		length = 13 + r.Intn(48)
	default:
		length = 61 + r.Intn(140) // up to 200 digits
	}
	if length == 1 {
		return string(byte('0' + r.Intn(2)))
	}
	b := make([]byte, length)
	b[0] = '1' // no leading zeros
	for i := 1; i < length; i++ {
		b[i] = byte('0' + r.Intn(2))
	}
	return string(b)
}

// genExpr builds a random expression string. Sub-expressions are parenthesized
// only sometimes — the string's own parse (per the grammar) defines its value,
// so unparenthesized nesting simply exercises precedence handling.
func genExpr(r *rand.Rand, depth int) string {
	if depth == 0 || r.Intn(10) == 0 {
		return genLiteral(r)
	}
	sp := func() string { // random inter-token spacing
		return []string{"", " ", "  ", "\t"}[r.Intn(4)]
	}
	wrap := func(s string) string {
		if r.Intn(10) < 6 {
			return "(" + sp() + s + sp() + ")"
		}
		return s
	}
	switch p := r.Intn(10); {
	case p < 4: // + or -
		op := []string{"+", "-"}[r.Intn(2)]
		return wrap(genExpr(r, depth-1)) + sp() + op + sp() + wrap(genExpr(r, depth-1))
	case p < 6: // *
		return wrap(genExpr(r, depth-1)) + sp() + "*" + sp() + wrap(genExpr(r, depth-1))
	case p < 8: // /
		return wrap(genExpr(r, depth-1)) + sp() + "/" + sp() + wrap(genExpr(r, depth-1))
	default: // unary minus
		return "-" + sp() + wrap(genExpr(r, depth-1))
	}
}

// ---------- Candidate runner ----------

// FailDetail describes the first failing case of a judged submission.
type FailDetail struct {
	Input    string `json:"input"`
	Expected string `json:"expected"`
	Got      string `json:"got"`
	Error    string `json:"error,omitempty"` // runtime error / timeout, if any
}

// Verdict is the outcome of judging one submission against all hidden cases.
type Verdict struct {
	Compiled      bool        `json:"compiled"`
	CompileOutput string      `json:"compile_output,omitempty"`
	Passed        int         `json:"passed"`
	Total         int         `json:"total"`
	FirstFail     *FailDetail `json:"first_fail,omitempty"`
}

// AllPassed reports whether every hidden case passed.
func (v Verdict) AllPassed() bool { return v.Compiled && v.Total > 0 && v.Passed == v.Total }

// Format renders a verdict as the feedback string shown to a solver (human or
// agent) — identical information for both.
func (v Verdict) Format() string {
	if !v.Compiled {
		return "COMPILE ERROR:\n" + v.CompileOutput
	}
	if v.AllPassed() {
		return fmt.Sprintf("PASS: all %d hidden tests passed.", v.Total)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "FAIL: %d/%d hidden tests passed.\n", v.Passed, v.Total)
	if v.FirstFail != nil {
		fmt.Fprintf(&b, "First failing case:\n  input:    %s\n", truncateForFeedback(v.FirstFail.Input))
		fmt.Fprintf(&b, "  expected: %s\n", truncateForFeedback(v.FirstFail.Expected))
		if v.FirstFail.Error != "" {
			fmt.Fprintf(&b, "  error:    %s\n", truncateForFeedback(v.FirstFail.Error))
		} else {
			fmt.Fprintf(&b, "  got:      %s\n", truncateForFeedback(v.FirstFail.Got))
		}
	}
	return b.String()
}

func truncateForFeedback(s string) string {
	const max = 600
	if len(s) > max {
		return s[:max] + "…[truncated]"
	}
	return s
}

// compile writes source into a temp module and builds it, returning the binary
// path, a cleanup func, and compiler output on failure.
func compile(ctx context.Context, source string) (bin string, cleanup func(), compileOut string, err error) {
	dir, err := os.MkdirTemp("", "benchsol-*")
	if err != nil {
		return "", nil, "", err
	}
	cleanup = func() { os.RemoveAll(dir) }
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0o644); err != nil {
		cleanup()
		return "", nil, "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module candidate\n\ngo 1.22\n"), 0o644); err != nil {
		cleanup()
		return "", nil, "", err
	}
	bin = filepath.Join(dir, "sol")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, ".")
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		compileOut = out.String()
		cleanup()
		return "", nil, compileOut, fmt.Errorf("build failed")
	}
	return bin, cleanup, "", nil
}

// runOnce executes a built binary on one input with a per-case timeout.
func runOnce(ctx context.Context, bin, input string, timeout time.Duration) (stdout, stderr string, err error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin)
	cmd.Stdin = strings.NewReader(input + "\n")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err = cmd.Run()
	if cctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("time limit exceeded (%s)", timeout)
	}
	return out.String(), errb.String(), err
}

// Judge compiles a candidate source and runs it against every hidden case,
// stopping detail collection at the first failure (but still counting passes).
func Judge(ctx context.Context, source string) (Verdict, error) {
	bin, cleanup, compileOut, err := compile(ctx, source)
	if err != nil {
		if compileOut != "" {
			return Verdict{Compiled: false, CompileOutput: compileOut}, nil
		}
		return Verdict{}, err
	}
	defer cleanup()

	cases := Cases()
	v := Verdict{Compiled: true, Total: len(cases)}
	for _, c := range cases {
		expected, oerr := EvalExpr(c.Input)
		if oerr != nil {
			return Verdict{}, fmt.Errorf("oracle failed on its own case %q: %v", c.Input, oerr)
		}
		stdout, stderrOut, rerr := runOnce(ctx, bin, c.Input, 10*time.Second)
		got := strings.TrimSpace(stdout)
		if rerr != nil {
			if v.FirstFail == nil {
				msg := rerr.Error()
				if s := strings.TrimSpace(stderrOut); s != "" {
					msg += ": " + s
				}
				v.FirstFail = &FailDetail{Input: c.Input, Expected: expected, Got: got, Error: msg}
			}
			continue
		}
		if got == expected {
			v.Passed++
		} else if v.FirstFail == nil {
			v.FirstFail = &FailDetail{Input: c.Input, Expected: expected, Got: got}
		}
	}
	return v, nil
}

// RunProgram compiles a candidate source and runs it on a single custom input —
// the debugging channel available to a solver (human or agent) alongside Judge.
func RunProgram(ctx context.Context, source, input string) string {
	bin, cleanup, compileOut, err := compile(ctx, source)
	if err != nil {
		if compileOut != "" {
			return "COMPILE ERROR:\n" + compileOut
		}
		return "harness error: " + err.Error()
	}
	defer cleanup()
	stdout, stderrOut, rerr := runOnce(ctx, bin, input, 10*time.Second)
	var b strings.Builder
	fmt.Fprintf(&b, "stdout:\n%s\n", stdout)
	if strings.TrimSpace(stderrOut) != "" {
		fmt.Fprintf(&b, "stderr:\n%s\n", stderrOut)
	}
	if rerr != nil {
		fmt.Fprintf(&b, "exit: %v\n", rerr)
	}
	return b.String()
}
