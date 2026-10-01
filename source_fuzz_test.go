package metricsql

import (
	"errors"
	"testing"
)

// FuzzParseSource checks that Parse never panics and that source positions are always
// valid: errors carry an in-range position and parsed expressions satisfy all
// provenance invariants checked by provenanceErrors.
//
// Run a long campaign with:
//
//	go test -run '^$' -fuzz=FuzzParseSource -fuzztime=1h .
func FuzzParseSource(f *testing.F) {
	for _, q := range upstreamTestQueries(f, "TestParseSuccess", "same", "another") {
		f.Add(q)
	}
	for _, q := range upstreamTestQueries(f, "TestParseError", "f") {
		f.Add(q)
	}
	f.Add("foo + foo")
	f.Add("WITH (x = rate(foo[5m])) sum(x) + x")
	f.Add(`foo{a="日本語" or b="ü"} keep_metric_names`)

	f.Fuzz(func(t *testing.T, q string) {
		e, err := Parse(q)
		if err != nil {
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("%q: expected *ParseError, got %T: %s", q, err, err)
			}
			if !validSpan(q, pe.Pos) {
				t.Fatalf("%q: error position %v is out of range", q, pe.Pos)
			}
			return
		}
		for _, msg := range provenanceErrors(q, e) {
			t.Error(msg)
		}
	})
}
