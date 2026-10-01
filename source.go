package metricsql

import (
	"errors"
	"fmt"
)

// Span represents a half-open [Start, End) byte range inside the parsed query.
type Span struct {
	Start int
	End   int
}

// IsZero returns true if s is the zero Span.
func (s Span) IsZero() bool {
	return s.Start == 0 && s.End == 0
}

func (s Span) contains(o Span) bool {
	return o.Start >= s.Start && o.End <= s.End
}

// SourceInfo contains information about the location of the expression in the parsed query.
//
// It is embedded into all the Expr types returned by Parse.
type SourceInfo struct {
	// Pos is the position of the expression in the parsed query.
	//
	// Expressions expanded from WITH templates point to the template use-site,
	// e.g. for `WITH (f(x) = rate(x[5m])) f(foo)` the `rate(...)` FuncExpr points to `f(foo)`,
	// while `foo` points to its own position.
	//
	// Expressions produced by constant folding point to the whole folded expression.
	Pos Span

	// Origin is the position of the expression inside the WITH template definition it was expanded from.
	//
	// It is the zero Span if the expression wasn't expanded from a WITH template
	// or if it was expanded from a built-in template.
	Origin Span

	// Parens contains positions of the parentheses, which were wrapping the expression
	// and were removed by Parse. The innermost parentheses go first.
	Parens []Span
}

// Source returns si.
func (si *SourceInfo) Source() *SourceInfo {
	return si
}

func (si *SourceInfo) addParens(s Span) {
	// Use full slice expression in order to avoid overwriting Parens shared with copies of the expression.
	si.Parens = append(si.Parens[:len(si.Parens):len(si.Parens)], s)
}

// OuterPos returns the position of the expression including the removed parentheses.
func (si *SourceInfo) OuterPos() Span {
	if len(si.Parens) > 0 {
		return si.Parens[len(si.Parens)-1]
	}
	return si.Pos
}

// SourceNode is implemented by all the Expr types returned by Parse.
type SourceNode interface {
	Source() *SourceInfo
}

// SourceOf returns the source information for e.
//
// It returns nil if e doesn't contain source information.
func SourceOf(e Expr) *SourceInfo {
	if sn, ok := e.(SourceNode); ok {
		return sn.Source()
	}
	return nil
}

func posOf(e Expr) Span {
	if si := SourceOf(e); si != nil {
		return si.OuterPos()
	}
	if me, ok := e.(*ModifierExpr); ok {
		return me.Pos
	}
	return Span{}
}

// ParseError is returned by Parse on invalid query.
type ParseError struct {
	// Msg is the error message.
	Msg string

	// Pos is the position of the query fragment the error refers to.
	Pos Span
}

// Error implements error interface.
func (pe *ParseError) Error() string {
	return pe.Msg
}

func newParseError(pos Span, format string, args ...any) *ParseError {
	return &ParseError{
		Msg: fmt.Sprintf(format, args...),
		Pos: pos,
	}
}

// withErrorPos attaches pos to err unless err already carries a position.
//
// It is used for errors returned while expanding a WITH template call, so the error
// points at the call instead of the whole query.
func withErrorPos(err error, pos Span) error {
	var pe *ParseError
	if errors.As(err, &pe) || pos.Start < 0 || pos.IsZero() {
		return err
	}
	return newParseError(pos, "%s", err)
}

// defaultTemplateBase is the base offset used for positions inside built-in WITH templates.
//
// It makes positions of built-in template expressions negative, so they can be distinguished
// from the positions inside the parsed query.
const defaultTemplateBase = -1 << 30

// withParens returns a copy of e with additional parens at s.
func withParens(e Expr, s Span) Expr {
	switch t := e.(type) {
	case *BinaryOpExpr:
		c := *t
		c.addParens(s)
		return &c
	case *FuncExpr:
		c := *t
		c.addParens(s)
		return &c
	case *AggrFuncExpr:
		c := *t
		c.addParens(s)
		return &c
	case *RollupExpr:
		c := *t
		c.addParens(s)
		return &c
	case *MetricExpr:
		c := *t
		c.addParens(s)
		return &c
	case *NumberExpr:
		c := *t
		c.addParens(s)
		return &c
	case *StringExpr:
		c := *t
		c.addParens(s)
		return &c
	case *DurationExpr:
		c := *t
		c.addParens(s)
		return &c
	default:
		return e
	}
}

// templateRelocator moves expressions expanded from WITH template body to the template use-site.
type templateRelocator struct {
	wa  *withArgExpr
	use Span
}

func (tr *templateRelocator) isFromBody(pos Span) bool {
	if pos.IsZero() {
		// Synthetic expression created during template expansion.
		return true
	}
	if tr.wa.isDefault {
		return pos.Start < 0
	}
	return tr.wa.Pos.contains(pos)
}

func (tr *templateRelocator) relocatePos(pos Span) Span {
	if tr.isFromBody(pos) {
		return tr.use
	}
	return pos
}

func (tr *templateRelocator) relocateInfo(si SourceInfo) SourceInfo {
	if !tr.isFromBody(si.Pos) {
		return si
	}
	if si.Origin.IsZero() && !tr.wa.isDefault && !si.Pos.IsZero() {
		si.Origin = si.Pos
	}
	si.Pos = tr.use
	// Parentheses from the template body do not exist at the use-site.
	si.Parens = nil
	return si
}

func (tr *templateRelocator) relocateDuration(de *DurationExpr) *DurationExpr {
	if de == nil {
		return nil
	}
	c := *de
	c.SourceInfo = tr.relocateInfo(c.SourceInfo)
	return &c
}

func (tr *templateRelocator) relocateNumber(ne *NumberExpr) *NumberExpr {
	if ne == nil {
		return nil
	}
	c := *ne
	c.SourceInfo = tr.relocateInfo(c.SourceInfo)
	return &c
}

func (tr *templateRelocator) relocateString(se *StringExpr) *StringExpr {
	if se == nil {
		return nil
	}
	c := *se
	c.SourceInfo = tr.relocateInfo(c.SourceInfo)
	return &c
}

func (tr *templateRelocator) relocateArgs(args []Expr) []Expr {
	if args == nil {
		return nil
	}
	dst := make([]Expr, len(args))
	for i, arg := range args {
		dst[i] = tr.relocate(arg)
	}
	return dst
}

func (tr *templateRelocator) relocateLabelFilterss(lfss [][]LabelFilter) [][]LabelFilter {
	if lfss == nil {
		return nil
	}
	dst := make([][]LabelFilter, len(lfss))
	for i, lfs := range lfss {
		dst[i] = make([]LabelFilter, len(lfs))
		for j, lf := range lfs {
			lf.Pos = tr.relocatePos(lf.Pos)
			dst[i][j] = lf
		}
	}
	return dst
}

func (tr *templateRelocator) relocate(e Expr) Expr {
	switch t := e.(type) {
	case *BinaryOpExpr:
		c := *t
		c.SourceInfo = tr.relocateInfo(c.SourceInfo)
		c.Left = tr.relocate(c.Left)
		c.Right = tr.relocate(c.Right)
		c.GroupModifier.Pos = tr.relocatePos(c.GroupModifier.Pos)
		c.JoinModifier.Pos = tr.relocatePos(c.JoinModifier.Pos)
		c.JoinModifierPrefix = tr.relocateString(c.JoinModifierPrefix)
		if c.FillLeft == c.FillRight {
			c.FillLeft = tr.relocateNumber(c.FillLeft)
			c.FillRight = c.FillLeft
		} else {
			c.FillLeft = tr.relocateNumber(c.FillLeft)
			c.FillRight = tr.relocateNumber(c.FillRight)
		}
		return &c
	case *FuncExpr:
		c := *t
		c.SourceInfo = tr.relocateInfo(c.SourceInfo)
		c.Args = tr.relocateArgs(c.Args)
		return &c
	case *AggrFuncExpr:
		c := *t
		c.SourceInfo = tr.relocateInfo(c.SourceInfo)
		c.Args = tr.relocateArgs(c.Args)
		c.Modifier.Pos = tr.relocatePos(c.Modifier.Pos)
		return &c
	case *RollupExpr:
		c := *t
		c.SourceInfo = tr.relocateInfo(c.SourceInfo)
		c.Expr = tr.relocate(c.Expr)
		c.Window = tr.relocateDuration(c.Window)
		c.Step = tr.relocateDuration(c.Step)
		c.Offset = tr.relocateDuration(c.Offset)
		if c.At != nil {
			c.At = tr.relocate(c.At)
		}
		return &c
	case *MetricExpr:
		c := *t
		c.SourceInfo = tr.relocateInfo(c.SourceInfo)
		c.LabelFilterss = tr.relocateLabelFilterss(c.LabelFilterss)
		return &c
	case *NumberExpr:
		return tr.relocateNumber(t)
	case *StringExpr:
		return tr.relocateString(t)
	case *DurationExpr:
		return tr.relocateDuration(t)
	case *parensExpr:
		c := *t
		c.SourceInfo = tr.relocateInfo(c.SourceInfo)
		c.args = tr.relocateArgs(c.args)
		if tr.isFromBody(t.Pos) {
			// Parentheses from the template body do not exist at the use-site.
			c.synthetic = true
		}
		return &c
	default:
		return e
	}
}

// relocateTemplateExpr returns a copy of e with all the expressions originating from wa body moved to the use span.
func relocateTemplateExpr(e Expr, wa *withArgExpr, use Span) Expr {
	if wa.isArg {
		// Template args keep their own positions.
		return e
	}
	tr := &templateRelocator{
		wa:  wa,
		use: use,
	}
	return tr.relocate(e)
}
