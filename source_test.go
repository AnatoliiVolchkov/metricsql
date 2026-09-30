package metricsql

import (
	"errors"
	"testing"
)

func fragment(t *testing.T, q string, s Span) string {
	t.Helper()
	if s.Start < 0 || s.End > len(q) || s.Start > s.End {
		t.Fatalf("invalid span %v for query of length %d", s, len(q))
	}
	return q[s.Start:s.End]
}

func mustParse(t *testing.T, q string) Expr {
	t.Helper()
	e, err := Parse(q)
	if err != nil {
		t.Fatalf("cannot parse %q: %s", q, err)
	}
	return e
}

func checkPos(t *testing.T, q string, e Expr, want string) {
	t.Helper()
	si := SourceOf(e)
	if si == nil {
		t.Fatalf("missing source info for %T", e)
	}
	if got := fragment(t, q, si.Pos); got != want {
		t.Fatalf("unexpected position for %T %s\nquery: %q\ngot:  %q\nwant: %q", e, e.AppendString(nil), q, got, want)
	}
}

func checkOrigin(t *testing.T, q string, e Expr, want string) {
	t.Helper()
	si := SourceOf(e)
	if want == "" {
		if !si.Origin.IsZero() {
			t.Fatalf("unexpected origin for %T: %v", e, si.Origin)
		}
		return
	}
	if got := fragment(t, q, si.Origin); got != want {
		t.Fatalf("unexpected origin for %T\ngot:  %q\nwant: %q", e, got, want)
	}
}

func TestSourceSelectorAndRollup(t *testing.T) {
	q := `rate(foo{bar="baz"}[5m])`
	fe := mustParse(t, q).(*FuncExpr)
	checkPos(t, q, fe, q)
	re := fe.Args[0].(*RollupExpr)
	checkPos(t, q, re, `foo{bar="baz"}[5m]`)
	checkPos(t, q, re.Window, `5m`)
	me := re.Expr.(*MetricExpr)
	checkPos(t, q, me, `foo{bar="baz"}`)
	lfs := me.LabelFilterss[0]
	if got := fragment(t, q, lfs[0].Pos); got != "foo" {
		t.Fatalf("unexpected metric name position: %q", got)
	}
	if got := fragment(t, q, lfs[1].Pos); got != `bar="baz"` {
		t.Fatalf("unexpected filter position: %q", got)
	}
}

func TestSourceSubqueryOffsetAt(t *testing.T) {
	q := `max_over_time(sum(foo)[1h:5m] offset -5m @ 123)`
	fe := mustParse(t, q).(*FuncExpr)
	re := fe.Args[0].(*RollupExpr)
	checkPos(t, q, re, `sum(foo)[1h:5m] offset -5m @ 123`)
	checkPos(t, q, re.Window, `1h`)
	checkPos(t, q, re.Step, `5m`)
	checkPos(t, q, re.Offset, `-5m`)
	checkPos(t, q, re.At, `123`)
	checkPos(t, q, re.Expr, `sum(foo)`)
}

func TestSourceAggregation(t *testing.T) {
	q := `sum(rate(foo[5m])) by (job) limit 5 > 10`
	be := mustParse(t, q).(*BinaryOpExpr)
	checkPos(t, q, be, q)
	ae := be.Left.(*AggrFuncExpr)
	checkPos(t, q, ae, `sum(rate(foo[5m])) by (job) limit 5`)
	if got := fragment(t, q, ae.Modifier.Pos); got != `by (job)` {
		t.Fatalf("unexpected modifier position: %q", got)
	}
	checkPos(t, q, be.Right, `10`)

	q = `sum by (job) (foo)`
	ae = mustParse(t, q).(*AggrFuncExpr)
	checkPos(t, q, ae, q)
	if got := fragment(t, q, ae.Modifier.Pos); got != `by (job)` {
		t.Fatalf("unexpected modifier position: %q", got)
	}
}

func TestSourceBinaryOpPrecedence(t *testing.T) {
	q := `foo + bar * baz`
	be := mustParse(t, q).(*BinaryOpExpr)
	checkPos(t, q, be, q)
	checkPos(t, q, be.Left, `foo`)
	checkPos(t, q, be.Right, `bar * baz`)

	q = `a ^ b ^ c`
	be = mustParse(t, q).(*BinaryOpExpr)
	checkPos(t, q, be, q)
	checkPos(t, q, be.Right, `b ^ c`)

	q = `a * b + c`
	be = mustParse(t, q).(*BinaryOpExpr)
	checkPos(t, q, be, q)
	checkPos(t, q, be.Left, `a * b`)
}

func TestSourceBinaryOpModifiers(t *testing.T) {
	q := `x{job="foo"} + on(job) group_left(instance) prefix "p_" fill(0) y`
	be := mustParse(t, q).(*BinaryOpExpr)
	checkPos(t, q, be, q)
	if got := fragment(t, q, be.GroupModifier.Pos); got != `on(job)` {
		t.Fatalf("unexpected group modifier position: %q", got)
	}
	if got := fragment(t, q, be.JoinModifier.Pos); got != `group_left(instance)` {
		t.Fatalf("unexpected join modifier position: %q", got)
	}
	checkPos(t, q, be.JoinModifierPrefix, `"p_"`)
	checkPos(t, q, be.FillLeft, `0`)
	checkPos(t, q, be.Right, `y`)
}

func TestSourceParens(t *testing.T) {
	q := `((foo + bar)) * 2`
	be := mustParse(t, q).(*BinaryOpExpr)
	checkPos(t, q, be, q)
	inner := be.Left.(*BinaryOpExpr)
	checkPos(t, q, inner, `foo + bar`)
	si := SourceOf(inner)
	if len(si.Parens) != 2 {
		t.Fatalf("unexpected number of parens: %d", len(si.Parens))
	}
	if got := fragment(t, q, si.Parens[0]); got != `(foo + bar)` {
		t.Fatalf("unexpected inner parens: %q", got)
	}
	if got := fragment(t, q, si.OuterPos()); got != `((foo + bar))` {
		t.Fatalf("unexpected outer parens: %q", got)
	}

	q = `(foo, bar)`
	fe := mustParse(t, q).(*FuncExpr)
	checkPos(t, q, fe, q)
}

func TestSourceUnaryMinus(t *testing.T) {
	q := `-foo`
	be := mustParse(t, q).(*BinaryOpExpr)
	if !be.IsUnaryMinus() {
		t.Fatalf("expecting unary minus")
	}
	checkPos(t, q, be, q)
	checkPos(t, q, be.Right, `foo`)

	q = `foo - bar`
	be = mustParse(t, q).(*BinaryOpExpr)
	if be.IsUnaryMinus() {
		t.Fatalf("unexpected unary minus")
	}
}

func TestSourceConstantFolding(t *testing.T) {
	q := `foo > (1 + 2 * 3)`
	be := mustParse(t, q).(*BinaryOpExpr)
	ne := be.Right.(*NumberExpr)
	if ne.N != 7 {
		t.Fatalf("unexpected value: %v", ne.N)
	}
	checkPos(t, q, ne, `1 + 2 * 3`)
	if got := fragment(t, q, ne.OuterPos()); got != `(1 + 2 * 3)` {
		t.Fatalf("unexpected outer position: %q", got)
	}
	if ne.Text() != "" {
		t.Fatalf("unexpected text for folded number: %q", ne.Text())
	}

	q = `foo > 1e3`
	be = mustParse(t, q).(*BinaryOpExpr)
	if got := be.Right.(*NumberExpr).Text(); got != "1e3" {
		t.Fatalf("unexpected text: %q", got)
	}

	q = `label_set(foo, "a", "b" + "c")`
	fe := mustParse(t, q).(*FuncExpr)
	checkPos(t, q, fe.Args[2], `"b" + "c"`)
}

func TestSourceWithTemplate(t *testing.T) {
	q := `WITH (f(x) = rate(x[5m])) f(foo{job="a"})`
	fe := mustParse(t, q).(*FuncExpr)
	checkPos(t, q, fe, `f(foo{job="a"})`)
	checkOrigin(t, q, fe, `rate(x[5m])`)
	re := fe.Args[0].(*RollupExpr)
	checkPos(t, q, re, `f(foo{job="a"})`)
	checkOrigin(t, q, re, `x[5m]`)
	checkPos(t, q, re.Window, `f(foo{job="a"})`)
	checkOrigin(t, q, re.Window, `5m`)
	me := re.Expr.(*MetricExpr)
	checkPos(t, q, me, `foo{job="a"}`)
	checkOrigin(t, q, me, "")

	// The same template used multiple times.
	q = `WITH (f(x) = sum(x)) f(foo) + f(bar)`
	be := mustParse(t, q).(*BinaryOpExpr)
	checkPos(t, q, be.Left, `f(foo)`)
	checkPos(t, q, be.Right, `f(bar)`)
	checkPos(t, q, be.Left.(*AggrFuncExpr).Args[0], `foo`)
	checkPos(t, q, be.Right.(*AggrFuncExpr).Args[0], `bar`)

	// Nested templates.
	q = `WITH (a = foo{x="y"}, b = a + 1) b * 2`
	be = mustParse(t, q).(*BinaryOpExpr)
	checkPos(t, q, be, q[len(`WITH (a = foo{x="y"}, b = a + 1) `):])
	inner := be.Left.(*BinaryOpExpr)
	checkPos(t, q, inner, `b`)
	checkPos(t, q, inner.Left, `b`)
	// Origin points to the innermost template definition.
	checkOrigin(t, q, inner.Left, `foo{x="y"}`)
	if len(SourceOf(inner).Parens) != 0 {
		t.Fatalf("synthetic parens mustn't be recorded")
	}

	// Metric name template.
	q = `WITH (t = foo{a="b"}) t{c="d"}`
	me = mustParse(t, q).(*MetricExpr)
	checkPos(t, q, me, `t{c="d"}`)
	lfs := me.LabelFilterss[0]
	if got := fragment(t, q, lfs[len(lfs)-1].Pos); got != `c="d"` {
		t.Fatalf("unexpected filter position: %q", got)
	}
	if got := fragment(t, q, lfs[1].Pos); got != `t{c="d"}` {
		t.Fatalf("unexpected template filter position: %q", got)
	}

	// Duration template.
	q = `WITH (w = 5m) rate(foo[w])`
	fe = mustParse(t, q).(*FuncExpr)
	re = fe.Args[0].(*RollupExpr)
	checkPos(t, q, re.Window, `w`)
	checkOrigin(t, q, re.Window, `5m`)
}

func TestSourceBuiltinTemplate(t *testing.T) {
	q := `alias(foo, "bar")`
	fe := mustParse(t, q).(*FuncExpr)
	if fe.Name != "label_set" {
		t.Fatalf("unexpected function: %s", fe.Name)
	}
	checkPos(t, q, fe, q)
	checkOrigin(t, q, fe, "")
	checkPos(t, q, fe.Args[0], `foo`)
	checkPos(t, q, fe.Args[1], q)
	checkPos(t, q, fe.Args[2], `"bar"`)
}

func TestSourceOrFilters(t *testing.T) {
	q := `foo{a="b" or c=~"d"}`
	me := mustParse(t, q).(*MetricExpr)
	checkPos(t, q, me, q)
	if got := fragment(t, q, me.LabelFilterss[0][1].Pos); got != `a="b"` {
		t.Fatalf("unexpected filter position: %q", got)
	}
	if got := fragment(t, q, me.LabelFilterss[1][1].Pos); got != `c=~"d"` {
		t.Fatalf("unexpected filter position: %q", got)
	}
}

func TestSourceComments(t *testing.T) {
	q := "foo # comment\n + bar"
	be := mustParse(t, q).(*BinaryOpExpr)
	checkPos(t, q, be, q)
	checkPos(t, q, be.Right, `bar`)
}

func TestSourceBounds(t *testing.T) {
	queries := []string{
		`foo`,
		`{__name__="foo"}`,
		`{"foo.bar", job="x"}`,
		`foo.bar.baz{a="b"}`,
		`rate(foo)`,
		`rate(foo[$__interval])`,
		`rate(foo[1i])`,
		`sum(foo) offset 5m`,
		`foo[5m:]`,
		`foo[:5m]`,
		`-(foo + 1)`,
		`+foo`,
		`foo if bar`,
		`foo ifnot bar default 0`,
		`foo or bar unless baz`,
		`(foo + bar) keep_metric_names`,
		`abs(foo) keep_metric_names`,
		`sum(foo) without (a, b)`,
		`topk_max(3, foo)`,
		`quantiles("phi", 0.5, 0.9, foo)`,
		`histogram_quantile(0.9, sum(rate(foo_bucket[5m])) by (le))`,
		`label_replace(foo, "a", "$1", "b", "(.*)")`,
		`ru(free, max)`,
		`ttf(free)`,
		`range_median(foo)`,
		`foo @ end()`,
		`foo > bool 10`,
		`foo * on() group_right bar`,
		`foo + fill_left(1) fill_right(-2) bar`,
		`1Ki + 2MB + 0x10 + 1_000 + Inf`,
		`WITH (x = foo, y = bar) x + y`,
		`WITH (f(a, b) = a / b) f(sum(foo), count(foo))`,
		`with (commonFilters = {job="a"}) foo{commonFilters, x="y"}`,
		`union(foo, bar)`,
		"foo\n# comment\n",
	}
	for _, q := range queries {
		e := mustParse(t, q)
		VisitAll(e, func(expr Expr) {
			si := SourceOf(expr)
			var spans []Span
			if si != nil {
				spans = append(spans, si.Pos, si.Origin)
				spans = append(spans, si.Parens...)
			}
			switch x := expr.(type) {
			case *ModifierExpr:
				spans = append(spans, x.Pos)
			case *MetricExpr:
				for _, lfs := range x.LabelFilterss {
					for _, lf := range lfs {
						spans = append(spans, lf.Pos)
					}
				}
			}
			for _, s := range spans {
				if s.Start < 0 || s.End > len(q) || s.Start > s.End {
					t.Fatalf("invalid span %v for %T %s in %q", s, expr, expr.AppendString(nil), q)
				}
			}
		})
	}
}

func TestSourceParseError(t *testing.T) {
	f := func(q, want string) {
		t.Helper()
		_, err := Parse(q)
		if err == nil {
			t.Fatalf("expecting error for %q", q)
		}
		var pe *ParseError
		if !errors.As(err, &pe) {
			t.Fatalf("expecting *ParseError; got %T", err)
		}
		if got := fragment(t, q, pe.Pos); got != want {
			t.Fatalf("unexpected error position for %q\ngot:  %q\nwant: %q", q, got, want)
		}
	}
	f(`unknown_func(foo)`, `unknown_func(foo)`)
	f(`sum(foo) + unknown_func(bar)`, `unknown_func(bar)`)
	f(`foo{bar=}`, `}`)
	f(`foo ) bar`, `) bar`)
	f(`foo + $bar`, `$bar`)
}
