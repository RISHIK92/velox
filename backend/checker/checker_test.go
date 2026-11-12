package checker

import (
	"errors"
	"strings"
	"testing"

	"github.com/rishik92/velox/lld"
)

// P5-W29-S03 (EXEC-4, LLDQ-4): the checker against honest, imprecise and forged output.
func scenario(t *testing.T, methods ...string) lld.Scenario {
	t.Helper()
	calls := []string{}
	for _, m := range methods {
		calls = append(calls, `{"method":"`+m+`","args":[]}`)
	}
	s, err := lld.DecodeScenario([]byte(`{"calls":[` + strings.Join(calls, ",") + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func results(t *testing.T, lines ...string) []lld.ResultLine {
	t.Helper()
	out := []lld.ResultLine{}
	for _, l := range lines {
		r, err := lld.DecodeResultLine([]byte(l))
		if err != nil {
			t.Fatalf("%s: %v", l, err)
		}
		out = append(out, r)
	}
	return out
}

func check(t *testing.T, compare string, tolerance *float64) []lld.Check {
	t.Helper()
	if compare == "" {
		return nil
	}
	method := "get"
	return []lld.Check{{Method: &method, Compare: &compare, Tolerance: tolerance}}
}

func TestComparisons(t *testing.T) {
	tol := 0.01
	sc := scenario(t, "new", "get")
	cases := []struct {
		name, want, got, compare string
		tolerance                *float64
		pass                     bool
		reason                   Reason
	}{
		{"identical", `[1,{"a":2}]`, `[1,{"a":2}]`, "", nil, true, ""},
		{"float spelling", `0.3`, `0.29999999999999999`, "", nil, true, ""},
		{"int and float as numbers", `3`, `3.0`, "", nil, true, ""},
		{"map key order", `{"a":1,"b":2}`, `{"b":2,"a":1}`, "", nil, true, ""},
		{"order matters when exact", `[1,2]`, `[2,1]`, "", nil, false, WrongValue},
		{"unordered as a multiset", `[1,2,2]`, `[2,1,2]`, "unordered", nil, true, ""},
		{"unordered counts repeats", `[1,2,2]`, `[2,1,1]`, "unordered", nil, false, WrongValue},
		{"unordered nested maps", `[{"a":1},{"b":2}]`, `[{"b":2},{"a":1.0}]`, "unordered", nil, true, ""},
		{"within tolerance", `1.0`, `1.005`, "approx", &tol, true, ""},
		{"outside tolerance", `1.0`, `1.02`, "approx", &tol, false, WrongValue},
		{"exact float differs", `0.1`, `0.10001`, "", nil, false, WrongValue},
		{"int past 2^53, exact", `9007199254740993`, `9007199254740993`, "", nil, true, ""},
		{"int past 2^53, off by one", `9007199254740993`, `9007199254740992`, "", nil, false, WrongValue},
		{"null is not zero", `null`, `0`, "", nil, false, WrongValue},
		{"string is not a number", `"1"`, `1`, "", nil, false, WrongValue},
		// Python prints 1e+20 where JS prints the digits: the same number either way.
		{"1e20 and its digits, exact", `[1e20]`, `[100000000000000000000]`, "", nil, true, ""},
		{"1e20 and its digits, unordered", `[1e20, 2]`, `[2, 100000000000000000000]`, "unordered", nil, true, ""},
		{"approx's default tolerance", `1.0`, `1.0000000001`, "approx", nil, true, ""},
		{"approx's default is small", `1.0`, `1.000001`, "approx", nil, false, WrongValue},
		{"approx with negatives", `-100.0`, `-100.5`, "approx", &tol, true, ""},
		{"approx near zero (the floor)", `0.0`, `0.005`, "approx", &tol, true, ""},
		{"approx near zero, past the floor", `0.0`, `0.02`, "approx", &tol, false, WrongValue},
		{"-0 is 0", `0`, `-0`, "", nil, true, ""},
		{"out of range is never equal", `1e400`, `1e999`, "", nil, false, WrongValue},
		{"out of range in a list", `[1e400]`, `[1e999]`, "unordered", nil, false, WrongValue},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := results(t, `{"call":0,"value":null}`, `{"call":1,"value":`+c.want+`}`)
			out := `{"call":0,"value":null}` + "\n" + `{"call":1,"value":` + c.got + `}` + "\n"
			r, err := Check(sc, want, []byte(out), check(t, c.compare, c.tolerance))
			if err != nil {
				t.Fatal(err)
			}
			if r.Passed != c.pass {
				t.Fatalf("passed %v, want %v", r.Passed, c.pass)
			}
			if !c.pass && (r.FirstMismatch == nil || r.FirstMismatch.Reason != c.reason || r.FirstMismatch.Call != 1) {
				t.Errorf("mismatch %+v", r.FirstMismatch)
			}
		})
	}
}

func TestErrorsCompareOnlyAsErrors(t *testing.T) {
	sc := scenario(t, "new", "get")
	errWant := results(t, `{"call":0,"value":null}`, `{"call":1,"error":"ValueError: over"}`)
	valWant := results(t, `{"call":0,"value":null}`, `{"call":1,"value":1}`)
	for _, c := range []struct {
		name   string
		want   []lld.ResultLine
		got    string
		reason Reason
	}{
		{"any error message", errWant, `{"call":1,"error":"IllegalStateException: something else"}`, ""},
		{"a value for an error", errWant, `{"call":1,"value":1}`, ExpectedError},
		{"an error for a value", valWant, `{"call":1,"error":"x"}`, UnexpectedError},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, err := Check(sc, c.want, []byte(`{"call":0,"value":null}`+"\n"+c.got+"\n"), nil)
			if err != nil {
				t.Fatal(err)
			}
			if c.reason == "" && !r.Passed {
				t.Fatalf("failed: %+v", r.FirstMismatch)
			}
			if c.reason != "" && (r.Passed || r.FirstMismatch.Reason != c.reason) {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestForgedOutputFails(t *testing.T) {
	sc := scenario(t, "new", "get")
	want := results(t, `{"call":0,"value":null}`, `{"call":1,"value":1}`)
	good := `{"call":0,"value":null}` + "\n" + `{"call":1,"value":1}`
	for name, out := range map[string]string{
		"an extra line":        good + "\n" + `{"call":2,"value":1}` + "\n",
		"a missing line":       `{"call":0,"value":null}` + "\n",
		"nothing":              "",
		"a wrong call index":   `{"call":0,"value":null}` + "\n" + `{"call":0,"value":1}` + "\n",
		"an unknown key":       `{"call":0,"value":null}` + "\n" + `{"call":1,"value":1,"passed":true}` + "\n",
		"two values on a line": `{"call":0,"value":null}` + "\n" + `{"call":1,"value":1}{"call":1,"value":1}` + "\n",
		"not JSON":             `{"call":0,"value":null}` + "\n" + `PASS` + "\n",
		"blank lines between":  `{"call":0,"value":null}` + "\n\n" + `{"call":1,"value":1}` + "\n",
		"a huge line":          `{"call":0,"value":"` + strings.Repeat("x", 2<<20) + `"}` + "\n" + `{"call":1,"value":1}` + "\n",
		"value and error":      `{"call":0,"value":null}` + "\n" + `{"call":1,"value":1,"error":"x"}` + "\n",
		"nested too deep":      `{"call":0,"value":null}` + "\n" + `{"call":1,"value":` + strings.Repeat("[", 65) + strings.Repeat("]", 65) + `}` + "\n",
		"a number too long":    `{"call":0,"value":null}` + "\n" + `{"call":1,"value":` + strings.Repeat("9", 401) + `}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			r, err := Check(sc, want, []byte(out), nil)
			if err != nil {
				t.Fatal(err)
			}
			if r.Passed {
				t.Fatal("forged output passed")
			}
		})
	}
	// The honest output, with and without its final newline, and with \r\n endings (JSON
	// whitespace, so accepted: a Windows-style line changes nothing it says).
	for _, out := range []string{good, good + "\n", strings.ReplaceAll(good, "\n", "\r\n")} {
		r, err := Check(sc, want, []byte(out), nil)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Passed {
			t.Errorf("honest output failed: %+v", r.FirstMismatch)
		}
	}
}

func TestTheProblemsOwnFaultIsAnErrorNotAVerdict(t *testing.T) {
	sc := scenario(t, "new", "get")
	if _, err := Check(sc, results(t, `{"call":0,"value":null}`), nil, nil); !errors.Is(err, ErrProblem) {
		t.Fatal(err)
	}
}

func TestTheProblemsOwnDataIsChecked(t *testing.T) {
	sc := scenario(t, "new", "get")
	good := results(t, `{"call":0,"value":null}`, `{"call":1,"value":1}`)
	out := []byte(`{"call":0,"value":null}` + "\n" + `{"call":1,"value":1}` + "\n")
	get, items, exact, approx := "get", "items", "exact", "approx"
	tol := 0.01
	for name, c := range map[string]struct {
		expected []lld.ResultLine
		checks   []lld.Check
	}{
		"an expected line out of order": {[]lld.ResultLine{good[1], good[0]}, nil},
		"a check with no method":        {good, []lld.Check{{Compare: &exact}}},
		"two checks for one method":     {good, []lld.Check{{Method: &get, Compare: &exact}, {Method: &get, Compare: &exact}}},
		"a tolerance on an exact check": {good, []lld.Check{{Method: &items, Compare: &exact, Tolerance: &tol}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Check(sc, c.expected, out, c.checks); !errors.Is(err, ErrProblem) {
				t.Fatalf("got %v", err)
			}
		})
	}
	if _, err := Check(sc, good, out, []lld.Check{{Method: &get, Compare: &approx, Tolerance: &tol}}); err != nil {
		t.Fatal(err)
	}
}
