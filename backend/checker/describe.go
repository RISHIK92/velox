package checker

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/rishik92/velox/lld"
)

// Interval's Q-WD-17: why a run failed, for a test its player may see. Unlike Check's
// report it echoes the run's own output (the value it got, its error), so it is only
// ever asked for a visible test's run; a hidden test's stays a category (LLDQ-4).

// Bounds on a description: a message of at most maxMessage printable ASCII characters,
// each value in it cut to maxShown.
const (
	maxMessage = 200
	maxShown   = 70
)

// Describe says why a run's output failed at m, as Check found it: the call, what the
// reference had and what came back, and the 1-based line of the player's file an error
// surfaced at, as the harness reported it (0 when it didn't, and for a wrong value).
func Describe(scenario lld.Scenario, expected []lld.ResultLine, stdout []byte, m Mismatch) (string, int) {
	if m.Call >= len(scenario.Calls) || m.Call >= len(expected) {
		return "more result lines than calls", 0
	}
	at := fmt.Sprintf("call %d (%s): ", m.Call, *scenario.Calls[m.Call].Method)
	body := bytes.TrimSuffix(stdout, []byte("\n"))
	var lines [][]byte
	if len(body) > 0 {
		lines = bytes.Split(body, []byte("\n"))
	}
	if m.Call >= len(lines) {
		return at + "no result", 0
	}
	got, err := lld.DecodeResultLine(lines[m.Call])
	if err != nil || len(lines[m.Call]) > maxLine {
		return at + "output that isn't a result line", 0
	}
	if *got.Call != int64(m.Call) {
		return at + fmt.Sprintf("a result for call %d instead", *got.Call), 0
	}
	want := "an error"
	if e := expected[m.Call]; e.Error == nil {
		want = shown(e.Value)
	}
	if got.Error != nil {
		line := 0
		if got.Line != nil {
			line = int(*got.Line)
		}
		return Clean(at + "expected " + want + ", got " + cut(*got.Error)), line
	}
	return Clean(at + "expected " + want + ", got " + shown(got.Value)), 0
}

// shown is a value as compact JSON, cut to maxShown.
func shown(v json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, v) != nil {
		return "a value"
	}
	return cut(b.String())
}

func cut(s string) string {
	if len(s) > maxShown {
		return s[:maxShown-3] + "..."
	}
	return s
}

// Clean makes a message printable ASCII (curly quotes made straight) and at most
// maxMessage long.
func Clean(s string) string {
	out := make([]byte, 0, min(len(s), maxMessage))
	for _, r := range s {
		if len(out) == maxMessage {
			break
		}
		switch {
		case r >= 0x20 && r <= 0x7e:
			out = append(out, byte(r))
		case r == '\u2018' || r == '\u2019':
			// g++ quotes names this way.
			out = append(out, '\'')
		case r == '\u201c' || r == '\u201d':
			out = append(out, '"')
		default:
			out = append(out, '?')
		}
	}
	return string(out)
}
