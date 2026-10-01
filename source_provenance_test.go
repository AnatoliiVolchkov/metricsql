package metricsql

import (
	"errors"
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// Provenance oracle for the source positions added to the parser.
//
// For queries without WITH templates every expression must be recoverable from the
// query text: parsing q[OuterPos] on its own must produce an expression with the same
// string representation. This catches positions that are in range but point at the
// wrong place, e.g. the left `foo` of `foo + foo` for the right operand.
//
// Documented exceptions:
//   - Durations depend on context: `offset -5m` is a single DurationExpr but `-5m` alone
//     is a unary minus, `$__interval` is only valid as a duration. Duration fragments are
//     re-parsed as `foo offset <fragment>`. Fill values are signed literals in the same way
//     (`fill(-0)`) and are re-parsed as `a + fill(<fragment>) b`.
//   - Built-in templates (`ru()`, `alias()`, `range_median()`, ...) expand into synthetic
//     nodes that all point at the whole use-site, so they share their parent's span.
//   - Unary minus `-x` is parsed as `0 - x`; the synthetic `0` has an empty span placed
//     at the minus sign.
//   - `(a + b) keep_metric_names` keeps Pos on `a + b` (the expression without parens)
//     while OuterPos covers `(a + b) keep_metric_names`.
//   - In `a + b keep_metric_names % c` the modifier applies to `+` while `b % c` is the
//     right operand after precedence balancing, so the span of `b % c` contains the
//     modifier token. Such fragments are re-parsed without it.
//   - Upstream accepts `+1 offset 0 @ 0 @ 0` but rejects `1 offset 0 @ 0 @ 0`, so fragments
//     are also re-parsed with a leading unary plus, which doesn't change the expression.
//
// For queries with WITH templates expressions expanded from a template point to the
// template use-site, so only range, containment and origin invariants are checked.

// upstreamTestQueries returns the first string argument of every call to one of fns
// inside the test function named testName in parser_test.go, which is upstream's file.
func upstreamTestQueries(t testing.TB, testName string, fns ...string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := goparser.ParseFile(fset, "parser_test.go", nil, 0)
	if err != nil {
		t.Fatalf("cannot parse parser_test.go: %s", err)
	}
	var queries []string
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != testName {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok || len(ce.Args) == 0 {
				return true
			}
			id, ok := ce.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			for _, fn := range fns {
				if id.Name != fn {
					continue
				}
				if bl, ok := ce.Args[0].(*ast.BasicLit); ok && bl.Kind == token.STRING {
					if s, err := strconv.Unquote(bl.Value); err == nil {
						queries = append(queries, s)
					}
				}
			}
			return true
		})
	}
	if len(queries) == 0 {
		t.Fatalf("no queries found in %s", testName)
	}
	return queries
}

func hasWith(q string) bool {
	return strings.Contains(strings.ToLower(q), "with")
}

// keepMetricNamesTokens returns spans of keep_metric_names tokens in q, found with the
// parser's lexer, so `0Ykeep_metric_names` is split exactly like Parse splits it.
func keepMetricNamesTokens(q string) []Span {
	var spans []Span
	var lex lexer
	lex.Init(q)
	for lex.Next() == nil && !isEOF(lex.Token) {
		if isKeepMetricNames(lex.Token) {
			spans = append(spans, lex.Pos)
		}
	}
	return spans
}

func reparse(frag string) (string, error) {
	e, err := Parse(frag)
	if err != nil {
		return "", err
	}
	return string(e.AppendString(nil)), nil
}

// usesBuiltinTemplate reports whether q calls one of the built-in WITH templates.
// It uses the parser's lexer, so whitespace and comments are handled exactly like Parse does.
func usesBuiltinTemplate(q string) bool {
	names := map[string]bool{}
	for _, wa := range getDefaultWithArgExprs() {
		names[wa.Name] = true
	}
	var lex lexer
	lex.Init(q)
	prev := ""
	for lex.Next() == nil && !isEOF(lex.Token) {
		if lex.Token == "(" && names[strings.ToLower(prev)] {
			return true
		}
		prev = lex.Token
	}
	return false
}

func validSpan(q string, s Span) bool {
	return s.Start >= 0 && s.Start <= s.End && s.End <= len(q)
}

func spanContains(outer, inner Span) bool {
	return inner.Start >= outer.Start && inner.End <= outer.End
}

func childExprs(e Expr) []Expr {
	var c []Expr
	add := func(x Expr) {
		if x != nil {
			c = append(c, x)
		}
	}
	switch t := e.(type) {
	case *BinaryOpExpr:
		add(t.Left)
		add(t.Right)
		if t.JoinModifierPrefix != nil {
			add(t.JoinModifierPrefix)
		}
		if t.FillLeft != nil {
			add(t.FillLeft)
		}
		if t.FillRight != nil && t.FillRight != t.FillLeft {
			add(t.FillRight)
		}
	case *FuncExpr:
		c = append(c, t.Args...)
	case *AggrFuncExpr:
		c = append(c, t.Args...)
	case *RollupExpr:
		add(t.Expr)
		if t.Window != nil {
			add(t.Window)
		}
		if t.Step != nil {
			add(t.Step)
		}
		if t.Offset != nil {
			add(t.Offset)
		}
		add(t.At)
	}
	return c
}

// exprSpans returns all spans stored in e itself, excluding its children.
func exprSpans(e Expr) []Span {
	var spans []Span
	if si := SourceOf(e); si != nil {
		spans = append(spans, si.Pos, si.Origin)
		spans = append(spans, si.Parens...)
	}
	switch t := e.(type) {
	case *BinaryOpExpr:
		spans = append(spans, t.GroupModifier.Pos, t.JoinModifier.Pos)
	case *AggrFuncExpr:
		spans = append(spans, t.Modifier.Pos)
	case *MetricExpr:
		for _, lfs := range t.LabelFilterss {
			for _, lf := range lfs {
				spans = append(spans, lf.Pos)
			}
		}
	}
	return spans
}

func isUnaryMinusZero(parent, e Expr) bool {
	be, ok := parent.(*BinaryOpExpr)
	return ok && be.IsUnaryMinus() && be.Left == e
}

// provenanceErrors returns all provenance invariant violations for q.
func provenanceErrors(q string, root Expr) []string {
	var errs []string
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf("%q: ", q)+fmt.Sprintf(format, args...))
	}
	withQuery := hasWith(q)

	var walk func(parent, e Expr)
	walk = func(parent, e Expr) {
		si := SourceOf(e)
		if si == nil {
			fail("%T %s has no source info", e, e.AppendString(nil))
			return
		}
		for _, s := range exprSpans(e) {
			if !validSpan(q, s) {
				fail("invalid span %v in %T %s", s, e, e.AppendString(nil))
				return
			}
		}
		outer := si.OuterPos()
		if !spanContains(outer, si.Pos) {
			fail("%T %s Pos %v is outside OuterPos %v", e, e.AppendString(nil), si.Pos, outer)
		}
		prev := si.Pos
		for _, p := range si.Parens {
			if !spanContains(p, prev) || p == prev {
				fail("%T %s parens %v don't strictly wrap %v", e, e.AppendString(nil), p, prev)
			}
			prev = p
		}
		if si.Pos.Start == si.Pos.End && !isUnaryMinusZero(parent, e) {
			fail("%T %s has an empty span %v", e, e.AppendString(nil), si.Pos)
		}

		if parent != nil && !withQuery {
			if pp := SourceOf(parent).Pos; !spanContains(pp, outer) {
				fail("%T %s at %v is outside its parent %v", e, e.AppendString(nil), outer, pp)
			}
		}

		builtin := parent != nil && SourceOf(parent).Pos == si.Pos
		if builtin && !withQuery && !usesBuiltinTemplate(q) {
			fail("%T %s shares span %v with its parent", e, e.AppendString(nil), si.Pos)
		}
		switch {
		case withQuery:
		case builtin:
			// Expanded from a built-in template such as ru() or alias(): every synthetic
			// node points at the whole use-site, which equals the parent's span.
		case isUnaryMinusZero(parent, e):
			if want := SourceOf(parent).Pos.Start; si.Pos != (Span{Start: want, End: want}) {
				fail("synthetic unary minus zero at %v, want empty span at %d", si.Pos, want)
			}
		default:
			frag := q[outer.Start:outer.End]
			want := string(e.AppendString(nil))
			inner := q[si.Pos.Start:si.Pos.End]
			if _, ok := e.(*DurationExpr); ok {
				frag = inner
				// Durations depend on context (`offset -5m`, `$__interval`), check them as an offset.
				re, err := Parse("foo offset " + frag)
				if err != nil {
					fail("DurationExpr %s fragment %q isn't a valid offset: %s", want, frag, err)
				} else if got := string(re.(*RollupExpr).Offset.AppendString(nil)); got != want {
					fail("DurationExpr %s fragment %q parses as offset %s", want, frag, got)
				}
				break
			}
			if be, ok := parent.(*BinaryOpExpr); ok && (e == be.FillLeft || e == be.FillRight) {
				// Fill values are signed literals in context (`fill(-0)`), check them as one.
				frag = inner
				fe, err := Parse("a + fill(" + frag + ") b")
				if err != nil {
					fail("fill value %s fragment %q isn't a valid fill value: %s", want, frag, err)
				} else if got := string(fe.(*BinaryOpExpr).FillLeft.AppendString(nil)); got != want {
					fail("fill value %s fragment %q parses as %s", want, frag, got)
				}
				break
			}
			got, err := reparse(frag)
			if err != nil || got != want {
				// Upstream accepts some forms only after a unary plus, e.g. `+1 offset 0 @ 0 @ 0`
				// while `1 offset 0 @ 0 @ 0` is rejected. Unary plus is an identity.
				if g, e := reparse("+" + frag); e == nil && g == want {
					got, err = g, nil
				}
			}
			if err != nil || got != want {
				// `a + b keep_metric_names % c` applies the modifier to `+`, but `b % c` is the
				// right operand, so its span unavoidably contains the modifier token.
				// The same word may also be a metric name, so drop one occurrence at a time.
				for _, s := range keepMetricNamesTokens(frag) {
					if g, e := reparse(frag[:s.Start] + " " + frag[s.End:]); e == nil && g == want {
						got, err = g, nil
						break
					}
				}
			}
			if err != nil {
				fail("%T %s fragment %q doesn't parse: %s", e, want, frag, err)
			} else if got != want {
				fail("%T %s fragment %q parses as %s", e, want, frag, got)
			}
		}

		for _, c := range childExprs(e) {
			walk(e, c)
		}
	}
	walk(nil, root)
	return errs
}

func checkProvenance(t *testing.T, q string) Expr {
	t.Helper()
	e := mustParse(t, q)
	for _, err := range provenanceErrors(q, e) {
		t.Error(err)
	}
	return e
}

func TestSourceProvenanceUpstreamSuccessCorpus(t *testing.T) {
	queries := upstreamTestQueries(t, "TestParseSuccess", "same", "another")
	if len(queries) < 400 {
		t.Fatalf("expected the full upstream corpus, got %d queries", len(queries))
	}
	for _, q := range queries {
		checkProvenance(t, q)
	}
}

func TestSourceProvenanceUpstreamErrorCorpus(t *testing.T) {
	queries := upstreamTestQueries(t, "TestParseError", "f")
	if len(queries) < 200 {
		t.Fatalf("expected the full upstream corpus, got %d queries", len(queries))
	}
	for _, q := range queries {
		_, err := Parse(q)
		if err == nil {
			t.Errorf("%q: expected a parse error", q)
			continue
		}
		var pe *ParseError
		if !errors.As(err, &pe) {
			t.Errorf("%q: expected *ParseError, got %T: %s", q, err, err)
			continue
		}
		if pe.Msg == "" {
			t.Errorf("%q: empty error message", q)
		}
		if !validSpan(q, pe.Pos) {
			t.Errorf("%q: error position %v is out of range", q, pe.Pos)
		}
	}
}

func TestSourceProvenanceCategories(t *testing.T) {
	queries := []string{
		// simple selectors
		`foo`, `{__name__="foo"}`, `foo{a="b",c!="d",e=~"f",g!~"h"}`, `{a="b"}`, `foo{}`,
		// functions and nested functions
		`abs(foo)`, `abs(abs(abs(foo)))`, `clamp(foo, 0, 1)`, `round(rate(foo[5m]), 0.1)`,
		`histogram_quantile(0.9, sum by (le) (rate(foo_bucket[5m])))`,
		`label_replace(rate(foo[5m]), "a", "$1", "b", "(.+)")`,
		// binary expressions
		`foo + bar`, `foo + bar * baz - qux / quux`, `foo > bool 1`, `foo and bar or baz unless qux`,
		`foo atan2 bar`, `foo ^ 2 ^ 3`, `foo if bar`, `foo ifnot bar`, `foo default 0`,
		// aggregations
		`sum(foo)`, `sum by (a, b) (foo)`, `sum(foo) without (a)`, `topk(3, foo)`, `count_values("v", foo)`,
		`quantile(0.9, foo)`, `sum(foo) by (a) limit 3`,
		// vector matching
		`foo * on(a) bar`, `foo * ignoring(a) group_left(b) bar`, `foo / on(a) group_right(b, c) bar`,
		`foo + on(a) group_left(b) prefix "x_" bar`, `foo + fill(0) bar`, `foo + fill_left(1) fill_right(2) bar`,
		// range selectors and rollups
		`foo[5m]`, `rate(foo[5m])`, `rate(foo)`, `rate(foo[1i])`, `rate(foo[$__interval])`, `rollup(foo[1h])`,
		`increase(foo[1h:5m])`, `quantile_over_time(0.9, foo[5m])`,
		// offsets and @ modifier
		`foo offset 5m`, `rate(foo[5m] offset 1h)`, `foo offset -5m`, `foo @ 123`, `foo @ start()`,
		`rate(foo[5m] offset 1h @ end())`,
		// subqueries
		`max_over_time(rate(foo[5m])[1h:])`, `max_over_time(sum(foo)[1h:5m] offset 10m)`, `foo[5m:1m]`,
		// redundant and deeply nested parentheses
		`(foo)`, `((((foo))))`, `((foo + (bar))) * ((2))`, `sum(((foo)))`, `rate(((foo))[5m])`,
		// constant folding
		`1 + 2`, `foo > (1 + 2 * 3)`, `foo / (60 * 60)`, `(1 + 2) * foo`, `2 ^ 10 + foo`, `"a" + "b"`,
		// unary minus and plus
		`-foo`, `foo + -bar`, `-(foo + bar)`, `+foo`, `--foo`, `-1`,
		// repeated identical subexpressions, metric names and function calls
		`foo + foo`, `foo + foo + foo`, `rate(foo[5m]) / rate(foo[5m])`, `sum(foo) by (a) + sum(foo) by (a)`,
		`foo{a="b"} and foo{a="b"}`, `abs(foo) + abs(foo)`, `foo + bar + foo`,
		// whitespace and multiline
		"foo  +\tbar", "sum(\n  rate(\n    foo[5m]\n  )\n)", "foo\r\n+\r\nbar", "  foo  ", "\n\nfoo\n\n",
		// comments
		"foo # comment", "foo # c1\n + # c2\n bar", "# leading\nfoo", "sum(\n  foo # inner\n)",
		// quoted strings and escaped characters
		`foo{a="x\"y"}`, `foo{a='x\'y'}`, "foo{a=`raw\\n`}", `foo{a="tab\tnewline\n"}`, `label_set(foo, "a", "\u00e9")`,
		`{"foo.bar"}`, `{"foo.bar", job="a"}`, `foo{"a.b"="c"}`,
		// UTF-8 / multibyte text
		`foo{a="日本語"} + bar{b="ü"}`, `{"métrique_ünï"} + foo`, `label_set(foo, "名前", "値") + foo`,
		// MetricsQL specific syntax
		`foo{a="b" or c="d"}`, `abs(foo) keep_metric_names`, `(foo + bar) keep_metric_names`,
		`1Ki + 2MB + 0x10 + 1_000 + Inf + NaN`, `union(foo, bar)`, `(foo, bar)`, `ru(free, max)`,
		`alias(foo, "bar")`, `rollup_candlestick(foo)`, `sort_by_label(foo, "a")`, `range_median(foo)`,
	}
	for _, q := range queries {
		checkProvenance(t, q)
	}
}

func TestSourceRepeatedSubexpressions(t *testing.T) {
	q := `foo + foo`
	be := checkProvenance(t, q).(*BinaryOpExpr)
	if l, r := SourceOf(be.Left).Pos, SourceOf(be.Right).Pos; l != (Span{0, 3}) || r != (Span{6, 9}) {
		t.Fatalf("unexpected operand positions: left %v right %v", l, r)
	}

	q = `rate(foo[5m]) / rate(foo[5m])`
	be = checkProvenance(t, q).(*BinaryOpExpr)
	checkPos(t, q, be.Left, `rate(foo[5m])`)
	if got := SourceOf(be.Right).Pos; got != (Span{16, 29}) {
		t.Fatalf("right rate() at %v", got)
	}
	rm := be.Right.(*FuncExpr).Args[0].(*RollupExpr).Expr
	if got := SourceOf(rm).Pos; got != (Span{21, 24}) {
		t.Fatalf("right foo at %v", got)
	}

	q = `foo + bar + foo`
	be = checkProvenance(t, q).(*BinaryOpExpr)
	if got := SourceOf(be.Right).Pos; got != (Span{12, 15}) {
		t.Fatalf("last foo at %v", got)
	}

	q = `abs(abs(abs(foo)))`
	fe := checkProvenance(t, q).(*FuncExpr)
	checkPos(t, q, fe.Args[0], `abs(abs(foo))`)
	checkPos(t, q, fe.Args[0].(*FuncExpr).Args[0], `abs(foo)`)
}

func TestSourceWhitespaceAndMultiline(t *testing.T) {
	q := "sum(\n  rate(\n    foo[5m]\n  )\n)"
	ae := checkProvenance(t, q).(*AggrFuncExpr)
	checkPos(t, q, ae, q)
	checkPos(t, q, ae.Args[0], "rate(\n    foo[5m]\n  )")

	q = "  foo  "
	checkPos(t, q, checkProvenance(t, q), "foo")

	q = "foo\r\n+\r\nbar"
	be := checkProvenance(t, q).(*BinaryOpExpr)
	checkPos(t, q, be.Right, "bar")
}

func TestSourceQuotedAndMultibyte(t *testing.T) {
	q := `foo{a="x\"y", b='z\'w'}`
	me := checkProvenance(t, q).(*MetricExpr)
	lfs := me.LabelFilterss[0]
	if got := fragment(t, q, lfs[1].Pos); got != `a="x\"y"` {
		t.Fatalf("unexpected filter position: %q", got)
	}
	if got := fragment(t, q, lfs[2].Pos); got != `b='z\'w'` {
		t.Fatalf("unexpected filter position: %q", got)
	}

	q = `foo{a="日本語"} + bar{b="ü"}`
	be := checkProvenance(t, q).(*BinaryOpExpr)
	checkPos(t, q, be.Left, `foo{a="日本語"}`)
	checkPos(t, q, be.Right, `bar{b="ü"}`)
	if got := SourceOf(be.Right).Pos.Start; got != len(`foo{a="日本語"} + `) {
		t.Fatalf("positions must be byte offsets, got %d", got)
	}

	q = `{"métrique"} / foo`
	be = checkProvenance(t, q).(*BinaryOpExpr)
	checkPos(t, q, be.Left, `{"métrique"}`)
}

func TestSourceWithProvenance(t *testing.T) {
	// Expressions expanded from a template point to the use-site, Origin points to the definition.
	q := `WITH (x = rate(foo[5m])) sum(x)`
	ae := mustParse(t, q).(*AggrFuncExpr)
	checkPos(t, q, ae, `sum(x)`)
	arg := ae.Args[0].(*FuncExpr)
	checkPos(t, q, arg, `x`)
	checkOrigin(t, q, arg, `rate(foo[5m])`)
	re := arg.Args[0].(*RollupExpr)
	checkPos(t, q, re, `x`)
	checkOrigin(t, q, re, `foo[5m]`)

	// Each use of the same template maps to its own use-site.
	q = `WITH (x = rate(foo[5m])) x + x`
	be := mustParse(t, q).(*BinaryOpExpr)
	if l, r := SourceOf(be.Left).Pos, SourceOf(be.Right).Pos; fragment(t, q, l) != "x" || fragment(t, q, r) != "x" || l == r {
		t.Fatalf("template uses must map to distinct use-sites: left %v right %v", l, r)
	}
	checkOrigin(t, q, be.Left, `rate(foo[5m])`)
	checkOrigin(t, q, be.Right, `rate(foo[5m])`)

	// Template function arguments keep their own positions.
	q = `WITH (f(m) = rate(m[5m])) f(foo) / f(bar)`
	be = mustParse(t, q).(*BinaryOpExpr)
	checkPos(t, q, be.Left.(*FuncExpr).Args[0].(*RollupExpr).Expr, `foo`)
	checkPos(t, q, be.Right.(*FuncExpr).Args[0].(*RollupExpr).Expr, `bar`)

	// WITH inside a function argument.
	q = `clamp_min(a, with (y = foo) y)`
	fe := mustParse(t, q).(*FuncExpr)
	checkPos(t, q, fe.Args[1], `y`)
	checkOrigin(t, q, fe.Args[1], `foo`)

	// Nested WITH referencing an outer template.
	q = `with (x = foo) a * x + (with (y = x) y) / y`
	be = mustParse(t, q).(*BinaryOpExpr)
	div := be.Right.(*BinaryOpExpr)
	checkPos(t, q, div.Left, `y`)
	checkOrigin(t, q, div.Left, `foo`)
	if got := SourceOf(div.Right).Pos; fragment(t, q, got) != "y" || got.Start != len(q)-1 {
		t.Fatalf("outer y must be the plain metric at the end, got %v", got)
	}
}

func TestSourceWithInvariants(t *testing.T) {
	var queries []string
	for _, q := range upstreamTestQueries(t, "TestParseSuccess", "same", "another") {
		if hasWith(q) {
			queries = append(queries, q)
		}
	}
	queries = append(queries,
		`WITH (x = rate(foo[5m])) sum(x)`,
		`WITH (a = foo, b = a + 1, c = b * 2) c / a`,
		`WITH (f(x, y) = x / y) f(sum(foo), count(foo))`,
		`with (commonFilters = {job="a"}) foo{commonFilters, x="y"} + bar{commonFilters}`,
	)
	if len(queries) < 50 {
		t.Fatalf("too few WITH queries: %d", len(queries))
	}
	for _, q := range queries {
		e, err := Parse(q)
		if err != nil {
			t.Errorf("%q: %s", q, err)
			continue
		}
		for _, err := range provenanceErrors(q, e) {
			t.Error(err)
		}
		VisitAll(e, func(expr Expr) {
			si := SourceOf(expr)
			if si == nil || si.Origin.IsZero() {
				return
			}
			if si.Origin.Start == si.Origin.End {
				t.Errorf("%q: empty origin for %T", q, expr)
			}
			// The use-site must not overlap with the definition it was expanded from.
			if spanContains(si.Origin, si.Pos) {
				t.Errorf("%q: %T use-site %v lies inside its definition %v", q, expr, si.Pos, si.Origin)
			}
		})
	}
}
