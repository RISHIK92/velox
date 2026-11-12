package judge

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/rishik92/velox/engine"
)

// P5-W30-S05: a real job end to end: queued in Valkey, taken by a worker, run in the
// sandbox, its output held to its expected results by the checker. EXEC_INTEGRATION=1.
func TestARealJobEndToEnd(t *testing.T) {
	if os.Getenv("EXEC_INTEGRATION") != "1" {
		t.Skip("EXEC_INTEGRATION not set")
	}
	s := store(t)
	ctx := context.Background()
	program := `import json, sys
calls = json.load(sys.stdin)["calls"]
for i, c in enumerate(calls):
    print(json.dumps({"call": i, "value": None if c["method"] == "new" else sum(c["args"])}))
`
	scenario := `{"calls":[{"method":"new","args":[]},{"method":"add","args":[2,3]},{"method":"add","args":[4,5]}]}`
	mk := func(id string, second int) []byte {
		raw, _ := json.Marshal(map[string]any{
			"id": id, "priority": "ranked", "language": "python",
			"files":  []map[string]string{{"name": "main.py", "content": program}},
			"limits": map[string]int{"seconds": 5, "memoryMB": 128},
			"runs": []map[string]any{{
				"stdin": scenario,
				"expected": []map[string]any{
					{"call": 0, "value": nil}, {"call": 1, "value": 5}, {"call": 2, "value": second},
				},
			}},
			"submittedAt": "2026-11-12T10:00:00Z",
		})
		return raw
	}
	right, wrong := uuid(), uuid()
	for id, second := range map[string]int{right: 9, wrong: 10} {
		if err := s.Submit(ctx, "ranked", mk(id, second)); err != nil {
			t.Fatal(err)
		}
	}
	w := worker(s, &engine.Runner{WorkDir: t.TempDir(), Docker: "docker"}, 2)
	stop := serve(t, w)
	ok, bad := result(t, s, right), result(t, s, wrong)
	stop()
	if *ok.Status != "done" || !*ok.Runs[0].Report.Passed || *ok.Runs[0].Verdict != "ok" {
		t.Fatalf("right: %+v", ok)
	}
	if *bad.Runs[0].Report.Passed || *bad.Runs[0].Report.FirstMismatch.Call != 2 {
		t.Fatalf("wrong: %+v", bad.Runs[0].Report)
	}
	// Output wasn't asked for, so none came back (LLDQ-4).
	if ok.Runs[0].Stdout != nil {
		t.Error("output returned unasked")
	}
}
