package checker

import (
	"github.com/rishik92/velox/lld"

	"strings"
	"testing"
)

// Q-WD-17 (Interval): why a visible test failed, in words its player can act on, with
// the line of their file an error surfaced at; a wrong value has no line.
func TestDescribeSaysWhatWasExpectedAndWhatCameBack(t *testing.T) {
	sc := scenario(t, "new", "park")
	want := results(t, `{"call":0,"value":null}`, `{"call":1,"value":[1,"a"]}`)
	wantErr := results(t, `{"call":0,"value":null}`, `{"call":1,"error":"ValueError: full"}`)
	first := `{"call":0,"value":null}` + "\n"
	cases := []struct {
		name     string
		expected []lld.ResultLine
		out      string
		message  string
		line     int
	}{
		{"a wrong value", want, first + `{"call":1,"value":[1, "b"]}`, `call 1 (park): expected [1,"a"], got [1,"b"]`, 0},
		{"an error for a value, with its line", want, first + `{"call":1,"error":"KeyError: 'x'","line":14}`, `call 1 (park): expected [1,"a"], got KeyError: 'x'`, 14},
		{"an error for a value, no line", want, first + `{"call":1,"error":"exception: boom"}`, `call 1 (park): expected [1,"a"], got exception: boom`, 0},
		{"a value for an error", wantErr, first + `{"call":1,"value":3}`, `call 1 (park): expected an error, got 3`, 0},
		{"no result for a call", want, `{"call":0,"value":null}`, `call 1 (park): no result`, 0},
		{"a result for another call", want, first + `{"call":4,"value":[1,"a"]}`, `call 1 (park): a result for call 4 instead`, 0},
		{"output that isn't a result line", want, first + `PASS`, `call 1 (park): output that isn't a result line`, 0},
		{"more lines than calls", want, first + `{"call":1,"value":[1,"a"]}` + "\n" + `{"call":2,"value":1}`, `more result lines than calls`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := Check(sc, c.expected, []byte(c.out+"\n"), nil)
			if err != nil || r.Passed {
				t.Fatalf("%+v %v", r, err)
			}
			message, line := Describe(sc, c.expected, []byte(c.out+"\n"), *r.FirstMismatch)
			if message != c.message || line != c.line {
				t.Fatalf("got %q at %d, want %q at %d", message, line, c.message, c.line)
			}
		})
	}
}

func TestDescribeKeepsItsMessageShortAndPrintable(t *testing.T) {
	sc := scenario(t, "new", "park")
	want := results(t, `{"call":0,"value":null}`, `{"call":1,"value":"`+strings.Repeat("a", 500)+`"}`)
	out := []byte(`{"call":0,"value":null}` + "\n" + `{"call":1,"value":"é` + strings.Repeat("b", 500) + `"}` + "\n")
	r, err := Check(sc, want, out, nil)
	if err != nil || r.Passed {
		t.Fatalf("%+v %v", r, err)
	}
	message, _ := Describe(sc, want, out, *r.FirstMismatch)
	if len(message) > 200 {
		t.Fatalf("%d bytes", len(message))
	}
	for _, c := range []byte(message) {
		if c < 0x20 || c > 0x7e {
			t.Fatalf("not printable: %q", message)
		}
	}
	if !strings.Contains(message, "expected \"aaa") || !strings.Contains(message, "got \"") {
		t.Fatalf("%q", message)
	}
}

func TestCleanStraightensQuotesAndCuts(t *testing.T) {
	if got := Clean("error: \u2018x\u2019 \u201cy\u201d\tz\u00e9"); got != `error: 'x' "y"?z?` {
		t.Fatalf("%q", got)
	}
	if got := Clean(strings.Repeat("a", 300)); len(got) != 200 {
		t.Fatalf("%d", len(got))
	}
}
