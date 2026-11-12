// Velox's execution engine (ported from Interval's services/exec, ADR-0041 there): runs
// untrusted code, one fresh sandbox per run. `run` and `check` serve local tooling
// (Interval's problem verification); `serve` runs the judge's workers on the queue the
// API's /judge fills.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rishik92/velox/checker"
	"github.com/rishik92/velox/engine"
	"github.com/rishik92/velox/judgejob"
	"github.com/rishik92/velox/lld"
	veloxRedis "github.com/rishik92/velox/shared/redis"
)

// What a run gets unless told otherwise; problems set their own from W30.
var defaults = engine.Limits{MemoryMB: 256, CPUs: 1, Pids: 64, TmpMB: 64, Seconds: 10, OutputKB: 1024}

func main() {
	var err error
	switch {
	case len(os.Args) > 1 && os.Args[1] == "run":
		err = run(os.Args[2:])
	case len(os.Args) > 1 && os.Args[1] == "serve":
		err = serve()
	case len(os.Args) > 1 && os.Args[1] == "check":
		var passed bool
		passed, err = check(os.Args[2:])
		if err == nil && !passed {
			os.Exit(2)
		}
	default:
		err = errors.New("usage: exec run --language L (--file F | --dir D) < stdin | exec check --scenario F --expected F --stdout F [--checks F] | exec serve")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "exec:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	language := flags.String("language", "python", "the submission's language: "+strings.Join(engine.Names(), ", "))
	file := flags.String("file", "", "the source file (the language's main file)")
	dir := flags.String("dir", "", "or a directory: every file in it, the harness's main file among them")
	seconds := flags.Int("seconds", defaults.Seconds, "the time limit")
	memory := flags.Int("memory", defaults.MemoryMB, "the memory limit, in MB")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if _, ok := engine.Lookup(*language); !ok {
		return fmt.Errorf("%w: %q", engine.ErrUnknownLanguage, *language)
	}
	files, err := readFiles(*file, *dir, *language)
	if err != nil {
		return err
	}
	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	r := &engine.Runner{WorkDir: os.Getenv("EXEC_WORK_DIR"), Docker: "docker", Runtime: os.Getenv("EXEC_RUNTIME")}
	res, err := r.Run(ctx, engine.Job{
		Language: *language,
		Files:    files,
		Stdin:    string(stdin),
		Limits:   limits(*seconds, *memory),
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(res)
}

// One file as the language's main file, or every regular file in a directory (a
// harness and a submission, P5-W29-S02).
func readFiles(file, dir, language string) (map[string]string, error) {
	if (file == "") == (dir == "") {
		return nil, errors.New("give --file or --dir")
	}
	if file != "" {
		lang, _ := engine.Lookup(language)
		source, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		return map[string]string{lang.Main: string(source)}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := map[string]string{}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		files[e.Name()] = string(body)
	}
	return files, nil
}

func limits(seconds, memoryMB int) engine.Limits {
	l := defaults
	l.Seconds, l.MemoryMB = seconds, memoryMB
	return l
}

// check compares a run's stdout with the reference's results (P5-W30-S01, EXEC-4):
// prints the report; false when it didn't pass. A fault in the problem's own data is an
// error (exit 1), never a verdict.
func check(args []string) (bool, error) {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	scenarioFile := flags.String("scenario", "", "the scenario (JSON)")
	expectedFile := flags.String("expected", "", "the reference's result lines (JSON array)")
	stdoutFile := flags.String("stdout", "", "the run's stdout")
	checksFile := flags.String("checks", "", "the problem's checks (JSON array), if any")
	if err := flags.Parse(args); err != nil {
		return false, err
	}
	if flags.NArg() > 0 {
		return false, fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	read := func(name string) ([]byte, error) {
		if name == "" {
			return nil, errors.New("--scenario, --expected and --stdout are required")
		}
		return os.ReadFile(name)
	}
	raw, err := read(*scenarioFile)
	if err != nil {
		return false, err
	}
	scenario, err := lld.DecodeScenario(raw)
	if err != nil {
		return false, fmt.Errorf("%w: scenario: %w", checker.ErrProblem, err)
	}
	var expected []lld.ResultLine
	if raw, err = read(*expectedFile); err != nil {
		return false, err
	}
	var lines []json.RawMessage
	if err := json.Unmarshal(raw, &lines); err != nil {
		return false, fmt.Errorf("%w: expected: %w", checker.ErrProblem, err)
	}
	for _, l := range lines {
		r, err := lld.DecodeResultLine(l)
		if err != nil {
			return false, fmt.Errorf("%w: expected: %w", checker.ErrProblem, err)
		}
		expected = append(expected, r)
	}
	var checks []lld.Check
	if *checksFile != "" {
		raw, err := os.ReadFile(*checksFile)
		if err != nil {
			return false, err
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return false, fmt.Errorf("%w: checks: %w", checker.ErrProblem, err)
		}
		for _, item := range items {
			c, err := lld.DecodeCheck(item)
			if err != nil {
				return false, fmt.Errorf("%w: checks: %w", checker.ErrProblem, err)
			}
			checks = append(checks, c)
		}
	}
	stdout, err := read(*stdoutFile)
	if err != nil {
		return false, err
	}
	report, err := checker.Check(scenario, expected, stdout, checks)
	if err != nil {
		return false, err
	}
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		return false, err
	}
	return report.Passed, nil
}

// serve runs the judge's workers until SIGTERM or SIGINT, finishing the jobs it holds
// (P5-W30-S05). Every setting comes from the environment: Velox's REDIS_ADDR and
// REDIS_PASSWORD (the same queue the API's /judge writes), EXEC_CONCURRENCY (by default
// the CPUs but one), EXEC_WORK_DIR, EXEC_RUNTIME.
func serve() error {
	var err error
	concurrency := max(runtime.NumCPU()-1, 1)
	if v := os.Getenv("EXEC_CONCURRENCY"); v != "" {
		if concurrency, err = strconv.Atoi(v); err != nil || concurrency < 1 || concurrency > 64 {
			return fmt.Errorf("EXEC_CONCURRENCY %q: 1 to 64", v)
		}
	}
	veloxRedis.Connect()
	client := veloxRedis.Client
	defer client.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	log.Info("judge: serving", "concurrency", concurrency)
	w := &judge.Worker{
		Store:       judge.Store{Client: client, Prefix: "judge"},
		Runner:      &engine.Runner{WorkDir: os.Getenv("EXEC_WORK_DIR"), Docker: "docker", Runtime: os.Getenv("EXEC_RUNTIME")},
		Concurrency: concurrency,
		// Past the longest job: 100 runs of 30 s would be far longer, but a real job's
		// runs are a scenario each, seconds apiece; a lease is renewed per job, W32.
		Lease: 10 * time.Minute,
		Log:   log,
	}
	return w.Serve(ctx)
}
