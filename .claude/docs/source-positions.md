# Source position model

What the fork adds to the parser, the invariants the tests enforce, and the API pint
depends on.

## Data model (`source.go`)

- **`Span{Start, End}`** is a half-open byte range `[Start, End)` inside the parsed query
  string.
- **`SourceInfo`** is embedded into every `Expr` type that `Parse` returns:
  - `Pos` is where the expression is in the query.
  - `Origin` is the position inside the WITH template definition the expression was
    expanded from. It is the zero `Span` when the expression didn't come from a WITH
    template, or came from a built-in one.
  - `Parens` lists the parentheses that wrapped the expression and were removed by `Parse`,
    innermost first. `OuterPos()` returns the span including them.
- **`SourceOf(e Expr) *SourceInfo`** returns the info for any expression.
- **`ParseError{Msg, Pos}`** is what `Parse` returns for invalid queries. `Pos` points at the
  fragment the error refers to:
  - an unexpected token;
  - an invalid regexp label filter (`cd22653`);
  - a WITH template call (`df330cc`);
  - an unsupported function.

  `withErrorPos` attaches a position only when the error doesn't carry one yet.

## Special cases (documented in `source_provenance_test.go`)

- **WITH templates.**
  - Expressions expanded from a template point to the template **use-site**. For
    `WITH (f(x) = rate(x[5m])) f(foo)`, the `rate(...)` node points to `f(foo)`, and `foo`
    points to itself.
  - `Origin` holds the position inside the template body.
- **Built-in templates** (`ru()`, `alias()`, `range_median()` and others) expand into
  synthetic nodes. They share their parent's span or use negative positions based at
  `defaultTemplateBase = -1 << 30`.
- **Constant folding.** A folded expression points to the whole folded source fragment.
- **Unary minus.** `-x` is parsed as `0 - x`. The synthetic `0` has an empty span at the
  minus sign.
- **`(a + b) keep_metric_names`.** `Pos` covers `a + b`, and `OuterPos` covers the whole text
  including the modifier.
- **Durations depend on context.** `offset -5m` is a single `DurationExpr`, but `-5m` alone is
  a unary minus. The provenance oracle re-parses duration fragments as
  `foo offset <fragment>`.

## Invariants enforced by tests

- For queries without WITH templates, every expression's `q[OuterPos()]` re-parses to an
  expression with the same `String()`. The provenance oracle reads every query from
  upstream's `TestParseSuccess` in `parser_test.go`, at least 400 of them. This catches
  spans that are in range but point at the wrong place, such as the left `foo` of
  `foo + foo` for the right operand.
- Every node returned by `Parse` has source info. A new node type without it fails with
  "has no source info".
- Ranges satisfy `0 <= Start <= End <= len(q)`, except for built-in template nodes.
- Errors from the upstream error corpus carry positions inside the query.
- Whitespace, newlines, comments, quoted strings and multibyte UTF-8 are covered.
  **Positions are bytes, not runes.**
- `FuzzParseSource` checks that no input panics and no span falls out of range.

## API pint depends on

pint's converter (`../pint/internal/parser/metricsql/metricsql.go`) uses:

- `Parse`, `*ParseError` (`Msg`, `Pos`), `Span`, `SourceOf` and the `Parens` field;
- the expression types `MetricExpr` (`LabelFilterss`), `RollupExpr` (`Window`, `Step`,
  `InheritStep`, `Offset`, `At`, `Pos`), `FuncExpr`, `AggrFuncExpr`, `BinaryOpExpr` (group and
  join modifiers, `KeepMetricNames`, fill values, `JoinModifierPrefix`), `NumberExpr`,
  `StringExpr` and `DurationExpr` (`AppendString`, `Duration`);
- the helpers `IsRollupFunc`, `IsSupportedFunction` and `GetRollupArgIdx`;
- for the implicit conversion warning (`../pint/internal/checks/promql_syntax_metricsql.go`):
  `IsLikelyInvalid`, `VisitAll`, `RollupExpr.ForSubquery`, `SourceInfo.Origin`, `OuterPos`
  and `AppendString`.

Renaming or changing any of these breaks pint. After a change here, run pint's tests
through `go.work` before tagging:

```sh
go test ./internal/parser/... ./internal/checks/... ./cmd/pint/
```

Run that in `../pint`.
