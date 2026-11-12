// Package redteam is EXEC-1's suite: attacks a sandbox must contain, run against any
// engine through Engine (P5-W28-S02, Q-P5-03). An engine passes only if every attack
// is contained: none reports an escape, each ends in a verdict it allows, and none
// errors. It reruns before every release (`pnpm redteam`).
package redteam

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/rishik92/velox/engine"
)

// Outcome is what an engine says about one run.
type Outcome struct {
	Verdict string
	Stdout  string
}

// Engine is anything that runs one program in a sandbox: services/exec here, Judge0
// for the comparison (W28-S04).
type Engine interface {
	Run(ctx context.Context, language, source, stdin string) (Outcome, error)
}

// Attack is a program that prints "ESCAPED: <what>" if it gets out, and the verdicts
// its contained ending may have.
type Attack struct {
	Name     string
	Language string
	Source   string
	Stdin    string
	Allowed  []string
	// Run at the same time on the same engine, as another submission would be.
	Beside *Attack
	// Ends at a limit rather than at DONE.
	Resource bool
	// Must report an escape in a deliberately weak sandbox (the negative control).
	CatchesWeak bool
}

// Unsafe says why an outcome breaks the sandbox, or "" when it is contained. canary is
// a value only the host's environment holds.
func Unsafe(a Attack, o Outcome, canary string) string {
	if i := strings.Index(o.Stdout, "ESCAPED"); i >= 0 {
		line, _, _ := strings.Cut(o.Stdout[i:], "\n")
		return line
	}
	if canary != "" && strings.Contains(o.Stdout, canary) {
		return "the host's environment reached the run"
	}
	if !slices.Contains(a.Allowed, o.Verdict) {
		return fmt.Sprintf("verdict %q, allowed %v", o.Verdict, a.Allowed)
	}
	if !a.Resource && !slices.Contains(strings.Split(o.Stdout, "\n"), "DONE") {
		return "the probe never finished its checks"
	}
	return ""
}

// ExecEngine runs attacks on services/exec with one fixed set of limits.
type ExecEngine struct{ Runner *engine.Runner }

// The suite's limits; the attacks' thresholds are worked out from them.
var Limits = engine.Limits{MemoryMB: 128, CPUs: 1, Pids: 64, TmpMB: 16, Seconds: 5, OutputKB: 64}

func NewExecEngine() ExecEngine {
	return ExecEngine{Runner: &engine.Runner{
		WorkDir: os.Getenv("EXEC_WORK_DIR"),
		Docker:  "docker",
		Runtime: os.Getenv("EXEC_RUNTIME"),
	}}
}

func (e ExecEngine) Run(ctx context.Context, language, source, stdin string) (Outcome, error) {
	lang, ok := engine.Lookup(language)
	if !ok {
		return Outcome{}, fmt.Errorf("%w: %q", engine.ErrUnknownLanguage, language)
	}
	res, err := e.Runner.Run(ctx, engine.Job{
		Language: language,
		Files:    map[string]string{lang.Main: source},
		Stdin:    stdin,
		Limits:   Limits,
	})
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Verdict: string(res.Verdict), Stdout: res.Stdout}, nil
}
