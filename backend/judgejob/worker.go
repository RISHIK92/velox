package judge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/rishik92/velox/checker"
	"github.com/rishik92/velox/engine"
	"github.com/rishik92/velox/lld"
)

// P5-W30-S05 (EXEC-2, EXEC-4, LOAD-3, LOAD-5; owner, 2026-11-12): the worker pool. Each
// of N goroutines claims a job (highest priority first, leased in the same step), runs
// each of its runs in the sandbox, holds output to its expected results with the
// checker, and stores the result; only then is the lease released. A fault of the
// engine or of the job's data is a "failed" result, never a crash, never blamed on the
// code; a fault storing the result keeps the lease, so the reaper hands the job on.

// Runner is the sandbox (engine.Runner), a port so the queue logic is tested without it.
type Runner interface {
	Run(ctx context.Context, job engine.Job) (engine.Result, error)
}

// Bounds on what a job may cost a worker, whoever submitted it.
const (
	maxJobBytes = 8 << 20
	// Output returned to the submitter, summed over a job's runs.
	maxReturnedOutput = 8 << 20
	maxError          = 500
	// Polled while idle: under the 10 s verdict target by a wide margin.
	idlePoll = 200 * time.Millisecond
	// A job's time beyond its runs' limits: starting, compiling, removing containers.
	perRunMargin = 30 * time.Second
)

type Worker struct {
	Store       Store
	Runner      Runner
	Concurrency int
	// The lease a claim starts with; a job's own lease is then set from its runs.
	Lease time.Duration
	Log   *slog.Logger
}

// Serve runs the pool until ctx ends; jobs already claimed are finished first.
func (w *Worker) Serve(ctx context.Context) error {
	var wg sync.WaitGroup
	for range w.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.reap(ctx)
	}()
	wg.Wait()
	return nil
}

func (w *Worker) reap(ctx context.Context) {
	tick := time.NewTicker(max(w.Lease/4, time.Second))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			back, broken, err := w.Store.Reap(context.WithoutCancel(ctx), now)
			switch {
			case err != nil:
				w.Log.Error("judge: reap", "error", err)
			case back > 0 || broken > 0:
				w.Log.Warn("judge: reaped", "requeued", back, "unreadableLeases", broken)
			}
		}
	}
}

func token() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (w *Worker) loop(ctx context.Context) {
	for ctx.Err() == nil {
		owner := token()
		kind, raw, priority, err := w.Store.Claim(ctx, owner, time.Now().Add(w.Lease))
		if err != nil || kind == "" {
			if err != nil && ctx.Err() == nil {
				w.Log.Error("judge: claim", "error", err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(idlePoll):
			}
			continue
		}
		// A claimed job is finished even if the pool is stopping meanwhile.
		w.handle(context.WithoutCancel(ctx), kind, raw, priority, owner)
	}
}

func (w *Worker) handle(ctx context.Context, kind string, raw []byte, priority, owner string) {
	claimed := time.Now()
	if kind == "bad" {
		w.Log.Error("judge: an unreadable job, dropped", "priority", priority, "bytes", len(raw))
		return
	}
	var probe struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &probe)
	id := probe.ID
	var result JudgeResult
	var job JudgeJob
	var err error
	switch {
	case len(raw) > maxJobBytes:
		result = failed(id, fmt.Sprintf("the job is %d bytes, over %d", len(raw), maxJobBytes))
	default:
		if job, err = DecodeJudgeJob(raw); err != nil {
			result = failed(id, "the job isn't valid: "+err.Error())
		}
	}
	if result.Status == nil {
		// Its own lease, from what it may take: every run at its limit, with a margin.
		budget := time.Duration(len(job.Runs)) * (time.Duration(*job.Limits.Seconds)*time.Second + perRunMargin)
		if _, err := w.Store.Extend(ctx, id, owner, time.Now().Add(budget)); err != nil {
			w.Log.Error("judge: extend", "id", id, "error", err)
		}
		result = w.process(ctx, job)
	}
	if err := result.Validate(); err != nil {
		result = failed(id, "the result broke its own contract: "+err.Error())
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		w.Log.Error("judge: result", "id", id, "error", err)
		return
	}
	first, err := w.Store.Finish(ctx, id, encoded)
	if err != nil {
		// Kept leased: the reaper takes it back for another worker when the lease runs out.
		w.Log.Error("judge: finish (left for the reaper)", "id", id, "error", err)
		return
	}
	if _, err := w.Store.Release(ctx, id, owner); err != nil {
		w.Log.Error("judge: release", "id", id, "error", err)
	}
	if !first {
		return
	}
	// One line per job, the basis of the verdict-latency metric (Phase 5's "Done when").
	queued := int64(0)
	if job.SubmittedAt != nil {
		if at, err := time.Parse(time.RFC3339Nano, *job.SubmittedAt); err == nil {
			queued = claimed.Sub(at).Milliseconds()
		}
	}
	language := ""
	if job.Language != nil {
		language = *job.Language
	}
	w.Log.Info("judge: job",
		"id", id, "priority", priority, "language", language, "runs", len(job.Runs),
		"status", *result.Status, "queueMs", queued, "runMs", time.Since(claimed).Milliseconds())
}

func failed(id, why string) JudgeResult {
	status := "failed"
	if len(why) > maxError {
		why = why[:maxError]
	}
	return JudgeResult{Id: &id, Status: &status, Error: &why, Runs: []RunResult{}}
}

func (w *Worker) process(ctx context.Context, job JudgeJob) JudgeResult {
	id := *job.Id
	files := map[string]string{}
	for _, f := range job.Files {
		files[*f.Name] = *f.Content
	}
	limits := engine.Limits{
		MemoryMB: int(*job.Limits.MemoryMB),
		Seconds:  int(*job.Limits.Seconds),
		CPUs:     1, Pids: 64, TmpMB: 64, OutputKB: 1024,
	}
	out := []RunResult{}
	returned := 0
	for i, run := range job.Runs {
		res, err := w.Runner.Run(ctx, engine.Job{Language: *job.Language, Files: files, Stdin: *run.Stdin, Limits: limits})
		if err != nil {
			return failed(id, fmt.Sprintf("run %d: the engine: %v", i, err))
		}
		verdict := string(res.Verdict)
		r := RunResult{Verdict: &verdict, TimeMs: &res.TimeMs}
		if run.Expected != nil {
			report, err := judged(run, res)
			if err != nil {
				return failed(id, fmt.Sprintf("run %d: %v", i, err))
			}
			r.Report = report
		}
		// Output only when asked (LLDQ-4), and only within the job's budget.
		if run.ReturnOutput != nil && *run.ReturnOutput && returned+len(res.Stdout)+len(res.Stderr) <= maxReturnedOutput {
			returned += len(res.Stdout) + len(res.Stderr)
			r.Stdout, r.Stderr = &res.Stdout, &res.Stderr
		}
		out = append(out, r)
	}
	status := "done"
	return JudgeResult{Id: &id, Status: &status, Runs: out}
}

// judged holds a run's output to its expected results: the run's stdin is the scenario
// the expected lines answer. A fault in that data is the job's, never the code's.
func judged(run Run, res engine.Result) (*Report, error) {
	scenario, err := lld.DecodeScenario([]byte(*run.Stdin))
	if err != nil {
		return nil, fmt.Errorf("%w: its stdin isn't a scenario: %w", checker.ErrProblem, err)
	}
	expected, err := convert(run.Expected, lld.DecodeResultLine)
	if err != nil {
		return nil, fmt.Errorf("%w: expected: %w", checker.ErrProblem, err)
	}
	checks, err := convert(run.Checks, lld.DecodeCheck)
	if err != nil {
		return nil, fmt.Errorf("%w: checks: %w", checker.ErrProblem, err)
	}
	if res.Verdict != engine.OK {
		// A run that didn't finish has no results to compare.
		passed := false
		return &Report{Passed: &passed}, nil
	}
	report, err := checker.Check(scenario, expected, []byte(res.Stdout), checks)
	if err != nil {
		return nil, err
	}
	r := &Report{Passed: &report.Passed}
	if m := report.FirstMismatch; m != nil {
		call, reason := int64(m.Call), string(m.Reason)
		r.FirstMismatch = &Mismatch{Call: &call, Reason: &reason}
	}
	return r, nil
}

// convert re-reads the job's values as the checker's own types, through JSON: the two
// are generated from the same zod definitions.
func convert[In any, Out any](in []In, decode func([]byte) (Out, error)) ([]Out, error) {
	out := make([]Out, 0, len(in))
	for _, v := range in {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		o, err := decode(raw)
		if err != nil {
			return nil, errors.Join(err)
		}
		out = append(out, o)
	}
	return out, nil
}
