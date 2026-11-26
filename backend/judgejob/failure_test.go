package judge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rishik92/velox/engine"
)

// sequence is a job's program that answers each run in turn with its own result.
type sequence struct {
	mu      sync.Mutex
	results []engine.Result
}

func (s *sequence) Prepare(context.Context, engine.Job) (engine.Program, error) { return s, nil }

func (s *sequence) Run(_ context.Context, _ string) (engine.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.results[0]
	s.results = s.results[1:]
	return r, nil
}

func (s *sequence) Close() {}

func javaJob(t *testing.T, runs ...map[string]any) JudgeJob {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"id": uuid(), "priority": "run", "language": "java",
		"files": []map[string]string{
			{"name": "Bank.java", "content": "public class Bank {}"},
			{"name": "Main.java", "content": "public class Main {}"},
		},
		"limits":      map[string]int{"seconds": 2, "memoryMB": 64},
		"runs":        runs,
		"submittedAt": "2026-11-24T10:00:00Z",
	})
	j, err := DecodeJudgeJob(raw)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

const twoCalls = `{"calls":[{"method":"new","args":[]},{"method":"get","args":[]}]}`

var wantOne = []map[string]any{{"call": 0, "value": nil}, {"call": 1, "value": 1}}

func run(name string) map[string]any {
	r := map[string]any{"stdin": twoCalls, "expected": wantOne}
	if name != "" {
		r["name"] = name
	}
	return r
}

func ok(stdout string) engine.Result { return engine.Result{Verdict: engine.OK, Stdout: stdout} }

// Interval's Q-WD-17 and LLDQ-4: a named run (a visible test) that fails says why, with
// the line of the player's file; an unnamed one (a hidden test) never does, however it
// failed.
func TestANamedRunThatFailsSaysWhyAndWhere(t *testing.T) {
	first := `{"call":0,"value":null}` + "\n"
	cases := []struct {
		name    string
		run     map[string]any
		result  engine.Result
		failure *Failure
	}{
		{"a wrong value", run("Test 1"), ok(first + `{"call":1,"value":2}` + "\n"),
			&Failure{Name: ptr("Test 1"), Message: ptr("call 1 (get): expected 1, got 2")}},
		{"an error, with its line", run("Test 2"), ok(first + `{"call":1,"error":"IllegalStateException: empty","line":14}` + "\n"),
			&Failure{Name: ptr("Test 2"), Message: ptr("call 1 (get): expected 1, got IllegalStateException: empty"), Line: ptr(int64(14))}},
		{"a compile error", run("Test 3"), engine.Result{Verdict: engine.CompileError, Stderr: "/code/Bank.java:7: error: ';' expected\n1 error\n"},
			&Failure{Name: ptr("Test 3"), Message: ptr("compile error: error: ';' expected"), Line: ptr(int64(7))}},
		{"a time limit", run("Test 4"), engine.Result{Verdict: engine.TimeLimit},
			&Failure{Name: ptr("Test 4"), Message: ptr("time limit: it ran too long")}},
		{"a compile time limit", run("Test 6"), engine.Result{Verdict: engine.CompileTimeLimit, Stderr: "partial warnings\n"},
			&Failure{Name: ptr("Test 6"), Message: ptr("compile time limit: it took too long to compile")}},
		{"passed", run("Test 5"), ok(first + `{"call":1,"value":1}` + "\n"), nil},
		{"hidden, a wrong value", run(""), ok(first + `{"call":1,"value":2}` + "\n"), nil},
		{"hidden, an error with a line", run(""), ok(first + `{"call":1,"error":"X","line":3}` + "\n"), nil},
		{"hidden, a compile error", run(""), engine.Result{Verdict: engine.CompileError, Stderr: "/code/Bank.java:7: error: x\n"}, nil},
		{"hidden, a crash", run(""), engine.Result{Verdict: engine.RuntimeError, Stderr: "Exception in thread \"main\" X\n\tat Bank.f(Bank.java:3)\n"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := &Worker{Runner: &sequence{results: []engine.Result{c.result}}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			got := w.process(context.Background(), javaJob(t, c.run))
			if err := got.Validate(); err != nil {
				t.Fatal(err)
			}
			if *got.Status != "done" || len(got.Runs) != 1 {
				t.Fatalf("%+v", got)
			}
			f := got.Runs[0].Failure
			if c.failure == nil {
				if f != nil {
					t.Fatalf("a failure for an unnamed or passing run: %+v", *f)
				}
				raw, _ := json.Marshal(got)
				if strings.Contains(string(raw), "failure") {
					t.Fatalf("%s", raw)
				}
				return
			}
			want, _ := json.Marshal(c.failure)
			have, _ := json.Marshal(f)
			if string(want) != string(have) {
				t.Fatalf("got %s, want %s", have, want)
			}
		})
	}
}

// Lines name the player's file only: with no single file beside the harness, a
// compile error's line is unknown.
func TestNoLineWithoutOnePlayersFile(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"id": uuid(), "priority": "run", "language": "java",
		"files":       []map[string]string{{"name": "Main.java", "content": "x"}},
		"limits":      map[string]int{"seconds": 2, "memoryMB": 64},
		"runs":        []map[string]any{run("Test 1")},
		"submittedAt": "2026-11-24T10:00:00Z",
	})
	j, err := DecodeJudgeJob(raw)
	if err != nil {
		t.Fatal(err)
	}
	w := &Worker{Runner: &sequence{results: []engine.Result{{Verdict: engine.CompileError, Stderr: "/code/Main.java:7: error: x\n"}}}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	f := w.process(context.Background(), j).Runs[0].Failure
	if f == nil || f.Line != nil {
		t.Fatalf("%+v", f)
	}
}

func ptr[T any](v T) *T { return &v }

// once is a sandbox that counts its compiles, runs and closes; each run gets verdict.
type once struct {
	mu                   sync.Mutex
	prepared, ran, close int
	verdict              engine.Verdict
	err                  error
}

func (o *once) Prepare(context.Context, engine.Job) (engine.Program, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.prepared++
	return o, o.err
}

func (o *once) Run(context.Context, string) (engine.Result, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ran++
	return engine.Result{Verdict: o.verdict}, nil
}

func (o *once) Close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.close++
}

// Owner, 2026-11-26: a job is compiled once for all its runs, and closed when done.
func TestAJobIsPreparedOnceForAllItsRuns(t *testing.T) {
	o := &once{verdict: engine.CompileTimeLimit}
	w := &Worker{Runner: o, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	got := w.process(context.Background(), javaJob(t, run("Test 1"), run(""), run("")))
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	if o.prepared != 1 || o.ran != 3 || o.close != 1 {
		t.Fatalf("prepared %d, ran %d, closed %d", o.prepared, o.ran, o.close)
	}
	for i, r := range got.Runs {
		if *r.Verdict != "compile_time_limit" || *r.Report.Passed {
			t.Errorf("run %d: %s", i, *r.Verdict)
		}
	}
	if f := got.Runs[0].Failure; f == nil || *f.Message != "compile time limit: it took too long to compile" {
		t.Errorf("%+v", f)
	}

	broken := &once{err: errors.New("docker is down")}
	w.Runner = broken
	got = w.process(context.Background(), javaJob(t, run("")))
	if *got.Status != "failed" || broken.ran != 0 {
		t.Fatalf("%+v ran %d", got, broken.ran)
	}
}

// A job's lease covers its compile, once, and each of its runs.
func TestALeaseCoversTheCompileOnceAndEveryRun(t *testing.T) {
	java := javaJob(t, run(""), run(""), run(""))
	if got, want := budget(java), 3*(2*time.Second+perRunMargin)+15*time.Second+perRunMargin; got != want {
		t.Errorf("java: %s, want %s", got, want)
	}
	python := java
	python.Language = ptr("python")
	if got, want := budget(python), 3*(2*time.Second+perRunMargin); got != want {
		t.Errorf("python: %s, want %s", got, want)
	}
}
