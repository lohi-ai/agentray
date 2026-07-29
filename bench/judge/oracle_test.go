package judge

// Oracle self-validation. The oracle is the benchmark's ground truth, so before
// anything is judged against it, its three independent components are checked
// by brute force / independent computation: negabinary conversion (round-trip
// over a dense integer range), floor division (against math.Floor), and the
// parser (hand-computed precedence facts + fully-parenthesized random trees).

import (
	"math"
	"math/big"
	"math/rand"
	"strings"
	"testing"
)

func TestNegabinaryKnownValues(t *testing.T) {
	known := map[int64]string{
		0: "0", 1: "1", 2: "110", 3: "111", 6: "11010",
		-1: "11", -2: "10", -3: "1101", 4: "100", 5: "101",
	}
	for n, want := range known {
		if got := ToNegabinary(big.NewInt(n)); got != want {
			t.Errorf("ToNegabinary(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestNegabinaryRoundTripDenseRange(t *testing.T) {
	for n := int64(-5000); n <= 5000; n++ {
		s := ToNegabinary(big.NewInt(n))
		if len(s) == 0 {
			t.Fatalf("empty representation for %d", n)
		}
		if len(s) > 1 && s[0] == '0' {
			t.Fatalf("leading zero in representation of %d: %q", n, s)
		}
		if strings.Trim(s, "01") != "" {
			t.Fatalf("non-binary digit in representation of %d: %q", n, s)
		}
		back, err := FromNegabinary(s)
		if err != nil {
			t.Fatalf("FromNegabinary(%q): %v", s, err)
		}
		if back.Int64() != n {
			t.Fatalf("round-trip failed: %d → %q → %d", n, s, back.Int64())
		}
	}
}

func TestNegabinaryRoundTripBig(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 200; i++ {
		v := new(big.Int).Rand(r, new(big.Int).Lsh(big.NewInt(1), 400))
		if r.Intn(2) == 0 {
			v.Neg(v)
		}
		s := ToNegabinary(v)
		back, err := FromNegabinary(s)
		if err != nil {
			t.Fatalf("FromNegabinary(%q): %v", s, err)
		}
		if back.Cmp(v) != 0 {
			t.Fatalf("big round-trip failed for %s", v)
		}
	}
}

func TestFloorDivAgainstMathFloor(t *testing.T) {
	for a := int64(-60); a <= 60; a++ {
		for b := int64(-60); b <= 60; b++ {
			if b == 0 {
				continue
			}
			want := int64(math.Floor(float64(a) / float64(b)))
			got := floorDiv(big.NewInt(a), big.NewInt(b)).Int64()
			if got != want {
				t.Fatalf("floorDiv(%d, %d) = %d, want %d", a, b, got, want)
			}
		}
	}
}

// TestEvalPrecedenceFacts checks hand-computed decimal facts through the full
// oracle pipeline (literals rendered by the independently-validated converter).
func TestEvalPrecedenceFacts(t *testing.T) {
	facts := []struct {
		expr string
		want int64
	}{
		{nb(2) + "+" + nb(3) + "*" + nb(4), 14},          // * before +
		{nb(2) + "*" + nb(3) + "+" + nb(4), 10},          //
		{nb(7) + "-" + nb(3) + "-" + nb(2), 2},           // left-assoc -
		{nb(100) + "/" + nb(7) + "/" + nb(2), 7},         // left-assoc /
		{"-" + nb(6) + "/" + nb(4), -2},                  // (−6)/4, unary binds tighter
		{"--" + nb(5), 5},                                // stacked unary
		{"-(" + nb(2) + "+" + nb(3) + ")*" + nb(4), -20}, // -( ) then *
		{nb(-7) + "/" + nb(2), -4},                       // floor
		{nb(7) + "/" + nb(-2), -4},
		{nb(-7) + "/" + nb(-2), 3},
		{nb(1) + "-" + nb(-1), 2},
	}
	for _, f := range facts {
		got, err := EvalExpr(f.expr)
		if err != nil {
			t.Fatalf("EvalExpr(%q): %v", f.expr, err)
		}
		want := ToNegabinary(big.NewInt(f.want))
		if got != want {
			t.Errorf("EvalExpr(%q) = %q, want %q (%d)", f.expr, got, want, f.want)
		}
	}
}

// exprTree is an independent expression representation: the test builds random
// trees, evaluates them directly, renders them FULLY parenthesized, and demands
// the oracle's string parse agree with the tree's own value.
type exprTree struct {
	op          byte // 0 = leaf, '+', '-', '*', '/', 'n' = unary neg
	val         *big.Int
	left, right *exprTree
}

func (e *exprTree) eval() *big.Int {
	switch e.op {
	case 0:
		return e.val
	case 'n':
		return new(big.Int).Neg(e.left.eval())
	case '+':
		return new(big.Int).Add(e.left.eval(), e.right.eval())
	case '-':
		return new(big.Int).Sub(e.left.eval(), e.right.eval())
	case '*':
		return new(big.Int).Mul(e.left.eval(), e.right.eval())
	default: // '/'
		return floorDiv(e.left.eval(), e.right.eval())
	}
}

func (e *exprTree) render() string {
	switch e.op {
	case 0:
		return ToNegabinary(e.val)
	case 'n':
		return "(-" + e.left.render() + ")"
	default:
		return "(" + e.left.render() + string(e.op) + e.right.render() + ")"
	}
}

func genTree(r *rand.Rand, depth int) *exprTree {
	if depth == 0 || r.Intn(4) == 0 {
		v := big.NewInt(r.Int63n(1_000_000) - 500_000)
		return &exprTree{val: v}
	}
	switch r.Intn(5) {
	case 0:
		return &exprTree{op: 'n', left: genTree(r, depth-1)}
	case 1:
		return &exprTree{op: '+', left: genTree(r, depth-1), right: genTree(r, depth-1)}
	case 2:
		return &exprTree{op: '-', left: genTree(r, depth-1), right: genTree(r, depth-1)}
	case 3:
		return &exprTree{op: '*', left: genTree(r, depth-1), right: genTree(r, depth-1)}
	default:
		right := genTree(r, depth-1)
		if right.eval().Sign() == 0 {
			right = &exprTree{val: big.NewInt(1)}
		}
		return &exprTree{op: '/', left: genTree(r, depth-1), right: right}
	}
}

func TestEvalAgainstRandomTrees(t *testing.T) {
	r := rand.New(rand.NewSource(99))
	for i := 0; i < 500; i++ {
		tree := genTree(r, 5)
		want := ToNegabinary(tree.eval())
		got, err := EvalExpr(tree.render())
		if err != nil {
			t.Fatalf("EvalExpr(%q): %v", tree.render(), err)
		}
		if got != want {
			t.Fatalf("tree %q: oracle %q, tree eval %q", tree.render(), got, want)
		}
	}
}

func TestCasesDeterministicAndValid(t *testing.T) {
	a, b := Cases(), Cases()
	if len(a) < 170 {
		t.Fatalf("only %d cases", len(a))
	}
	if len(a) != len(b) {
		t.Fatalf("nondeterministic case count: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("case %d differs across generations", i)
		}
		if _, err := EvalExpr(a[i].Input); err != nil {
			t.Fatalf("case %d does not evaluate: %q: %v", i, a[i].Input, err)
		}
		if len(a[i].Input) > 20000 {
			t.Fatalf("case %d exceeds the stated length bound", i)
		}
	}
}
