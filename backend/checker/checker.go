// Package checker compares a submission's raw results with the reference solution's,
// outside the sandbox (EXEC-4; Q-P5-07; P5-W29-S03). A submission shares its harness's
// process, so it can write anything to stdout: every line is parsed strictly, numbers
// compare as numbers, an error only as an error (its text differs by language), and
// nothing a run printed is ever echoed. The report names a category, never the output:
// what a player may see of it is the caller's to decide (LLDQ-4).
package checker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/rishik92/velox/lld"
)

// A reason is a category only.
type Reason string

const (
	Output          Reason = "output"           // not exactly the result lines asked for
	WrongValue      Reason = "wrong value"      // a value unlike the reference's
	UnexpectedError Reason = "unexpected error" // an error where the reference had a value
	ExpectedError   Reason = "expected error"   // a value where the reference errored
)

// Mismatch says where and why. The call index tells a player where in a scenario it
// failed, so for hidden tests the caller shows the category only (LLDQ-4). For output
// that has too many lines, Call is the number of calls.
type Mismatch struct {
	Call   int    `json:"call"`
	Reason Reason `json:"reason"`
}

type Report struct {
	Passed        bool      `json:"passed"`
	FirstMismatch *Mismatch `json:"firstMismatch,omitempty"`
}

// Bounds on what a submission may print, checked before any comparison: a line, a
// value's nesting, and a number's digits. Past them the output is refused, so a hostile
// result costs the checker little (it runs outside the sandbox, without its limits).
const (
	maxLine   = 1 << 20
	maxDepth  = 64
	maxDigits = 400
)

const defaultTolerance = 1e-9

func fail(call int, r Reason) Report {
	return Report{FirstMismatch: &Mismatch{Call: call, Reason: r}}
}

// ErrProblem is a fault in the problem's own data (recorded at publication), never the
// player's: it fails the check as an error, not as a verdict.
var ErrProblem = errors.New("checker: the problem's data")

// Check compares stdout with the reference's results for the same scenario.
func Check(scenario lld.Scenario, expected []lld.ResultLine, stdout []byte, checks []lld.Check) (Report, error) {
	byMethod, err := problem(scenario, expected, checks)
	if err != nil {
		return Report{}, err
	}
	n := len(scenario.Calls)
	body := bytes.TrimSuffix(stdout, []byte("\n"))
	var lines [][]byte
	if len(body) > 0 {
		lines = bytes.Split(body, []byte("\n"))
	}
	for i := 0; i < n; i++ {
		if i >= len(lines) {
			return fail(i, Output), nil
		}
		if len(lines[i]) > maxLine {
			return fail(i, Output), nil
		}
		got, err := lld.DecodeResultLine(lines[i])
		if err != nil || *got.Call != int64(i) || !bounded(got.Value) {
			return fail(i, Output), nil
		}
		want := expected[i]
		switch {
		case want.Error != nil && got.Error != nil:
		case want.Error != nil:
			return fail(i, ExpectedError), nil
		case got.Error != nil:
			return fail(i, UnexpectedError), nil
		default:
			same, err := matches(want.Value, got.Value, byMethod[*scenario.Calls[i].Method])
			if err != nil {
				return Report{}, err
			}
			if !same {
				return fail(i, WrongValue), nil
			}
		}
	}
	if len(lines) != n {
		return fail(n, Output), nil
	}
	return Report{Passed: true}, nil
}

// problem checks the problem's own data before a submission is judged against it: a
// fault there is ErrProblem, never the player's verdict.
func problem(scenario lld.Scenario, expected []lld.ResultLine, checks []lld.Check) (map[string]lld.Check, error) {
	if err := scenario.Validate(); err != nil {
		return nil, fmt.Errorf("%w: scenario: %w", ErrProblem, err)
	}
	if len(expected) != len(scenario.Calls) {
		return nil, fmt.Errorf("%w: %d expected results for %d calls", ErrProblem, len(expected), len(scenario.Calls))
	}
	for i, e := range expected {
		if err := e.Validate(); err != nil || *e.Call != int64(i) {
			return nil, fmt.Errorf("%w: expected result %d", ErrProblem, i)
		}
		if e.Value != nil {
			if _, err := decode(e.Value); err != nil {
				return nil, fmt.Errorf("%w: expected result %d: %w", ErrProblem, i, err)
			}
		}
	}
	byMethod := map[string]lld.Check{}
	for _, c := range checks {
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("%w: check: %w", ErrProblem, err)
		}
		if _, twice := byMethod[*c.Method]; twice {
			return nil, fmt.Errorf("%w: two checks for %s", ErrProblem, *c.Method)
		}
		if c.Tolerance != nil && *c.Compare != "approx" {
			return nil, fmt.Errorf("%w: a tolerance on a %s check of %s", ErrProblem, *c.Compare, *c.Method)
		}
		byMethod[*c.Method] = c
	}
	return byMethod, nil
}

// bounded says whether a submission's value is within the bounds, scanning its raw
// bytes once: nesting depth, and the length of every number.
func bounded(raw json.RawMessage) bool {
	if raw == nil {
		return true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return err == io.EOF
		}
		switch t := tok.(type) {
		case json.Delim:
			if t == '[' || t == '{' {
				depth++
				if depth > maxDepth {
					return false
				}
			} else {
				depth--
			}
		case json.Number:
			if len(t) > maxDigits {
				return false
			}
		}
	}
}

func matches(want, got json.RawMessage, c lld.Check) (bool, error) {
	a, err := decode(want)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrProblem, err)
	}
	b, err := decode(got)
	if err != nil {
		// Already decoded once as a result line: only a malformed value lands here.
		return false, nil
	}
	compare := "exact"
	if c.Compare != nil {
		compare = *c.Compare
	}
	if compare == "unordered" {
		return sameMultiset(a, b), nil
	}
	tolerance := 0.0
	if compare == "approx" {
		tolerance = defaultTolerance
		if c.Tolerance != nil {
			tolerance = *c.Tolerance
		}
	}
	return equal(a, b, tolerance), nil
}

func decode(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}

// Numbers compare as numbers: integers exactly (past 2^53 too), and anything with a
// fraction or exponent as float64, within a tolerance when one is set.
func equal(a, b any, tolerance float64) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case bool, string:
		return a == b
	case json.Number:
		y, ok := b.(json.Number)
		return ok && numbersEqual(x, y, tolerance)
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !equal(x[i], y[i], tolerance) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !equal(v, w, tolerance) {
				return false
			}
		}
		return true
	}
	return false
}

// An integer literal: JSON's own spelling is canonical (no leading zeros), but for -0.
func integer(n json.Number) (string, bool) {
	s := string(n)
	if strings.ContainsAny(s, ".eE") {
		return "", false
	}
	if s == "-0" {
		s = "0"
	}
	return s, true
}

func numbersEqual(a, b json.Number, tolerance float64) bool {
	ia, okA := integer(a)
	ib, okB := integer(b)
	if okA && okB && tolerance == 0 {
		return ia == ib
	}
	fa, errA := a.Float64()
	fb, errB := b.Float64()
	// Out of float64's range (1e400): unequal, never matched as infinities.
	if errA != nil || errB != nil {
		return false
	}
	if tolerance == 0 {
		return fa == fb
	}
	return math.Abs(fa-fb) <= tolerance*math.Max(1, math.Max(math.Abs(fa), math.Abs(fb)))
}

// One spelling per number, the same for every way of writing it: an integer value
// (3, 3.0, 1e20, 100000000000000000000) as its integer digits, anything else as Go's
// shortest float64. false when it is out of float64's range.
func numberKey(n json.Number) (string, bool) {
	if s, ok := integer(n); ok {
		return s, true
	}
	f, err := n.Float64()
	if err != nil {
		return "", false
	}
	if f == math.Trunc(f) {
		i, _ := new(big.Float).SetFloat64(f).Int(nil)
		return i.String(), true
	}
	return strconv.FormatFloat(f, 'g', -1, 64), true
}

// A list compared as a multiset: each element matched once, in any order.
func sameMultiset(a, b any) bool {
	x, okA := a.([]any)
	y, okB := b.([]any)
	if !okA || !okB {
		return equal(a, b, 0)
	}
	if len(x) != len(y) {
		return false
	}
	keys := func(vs []any) ([]string, bool) {
		out := make([]string, len(vs))
		for i, v := range vs {
			var b bytes.Buffer
			if !canonical(&b, v) {
				return nil, false
			}
			out[i] = b.String()
		}
		sort.Strings(out)
		return out, true
	}
	kx, okX := keys(x)
	ky, okY := keys(y)
	if !okX || !okY {
		return false
	}
	for i := range kx {
		if kx[i] != ky[i] {
			return false
		}
	}
	return true
}

// canonical writes one spelling per value into a shared buffer (linear in its size), so
// equal values sort and compare alike: numbers by numberKey, maps by sorted key. false
// for a number out of range.
func canonical(b *bytes.Buffer, v any) bool {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		b.WriteString(strconv.Quote(x))
	case json.Number:
		k, ok := numberKey(x)
		if !ok {
			return false
		}
		b.WriteString("n" + k)
	case []any:
		b.WriteByte('[')
		for _, e := range x {
			if !canonical(b, e) {
				return false
			}
			b.WriteByte(',')
		}
		b.WriteByte(']')
	case map[string]any:
		ks := make([]string, 0, len(x))
		for k := range x {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		b.WriteByte('{')
		for _, k := range ks {
			b.WriteString(strconv.Quote(k) + ":")
			if !canonical(b, x[k]) {
				return false
			}
			b.WriteByte(',')
		}
		b.WriteByte('}')
	default:
		return false
	}
	return true
}
