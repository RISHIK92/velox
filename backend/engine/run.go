package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// P5-W28-S01 (EXEC-3): one container, start to cleanup. The docker CLI is driven directly
// (os/exec), so the service has no dependencies: create, start attached with stdin
// piped, inspect, then always remove.

type Verdict string

const (
	OK           Verdict = "ok"
	RuntimeError Verdict = "runtime_error"
	TimeLimit    Verdict = "time_limit"
	MemoryLimit  Verdict = "memory_limit"
	OutputLimit  Verdict = "output_limit"
	CompileError Verdict = "compile_error"
	// The compile overran its own budget (Language.CompileSeconds): the code took too
	// long to compile, which is not the program's time limit.
	CompileTimeLimit Verdict = "compile_time_limit"
)

type Job struct {
	Language string
	Files    map[string]string
	Stdin    string
	Limits   Limits
}

type Result struct {
	Verdict  Verdict `json:"verdict"`
	ExitCode int     `json:"exitCode"`
	Stdout   string  `json:"stdout"`
	Stderr   string  `json:"stderr"`
	// The program's own running time, from the container's start to its exit; starting
	// the container isn't counted against it.
	TimeMs int64 `json:"timeMs"`
}

type Runner struct {
	// Where run directories are made. The Docker host must see the same path (the exec
	// service runs on the execution host itself, EXEC-5).
	WorkDir string
	Docker  string
	// "runsc" on the execution hosts (gVisor); empty uses Docker's default.
	Runtime string
}

var (
	ErrUnknownLanguage = errors.New("unknown language")
	errFileName        = errors.New("file name isn't a plain name")
	errNotRun          = errors.New("the container didn't run to its end")
)

// Starting a container (more so under gVisor) takes time the program never had; the
// wall-clock backstop allows for it, and the verdict uses the container's own clock.
const startAllowance = 10 * time.Second

// Each docker call but the attached start is bounded, so a wedged daemon fails the run
// instead of hanging it.
const dockerTimeout = 30 * time.Second

// capped keeps the first max bytes and calls over once when more arrive.
type capped struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	max  int
	over func()
	hit  bool
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.max - c.buf.Len()
	if len(p) > room {
		c.buf.Write(p[:max(room, 0)])
		if !c.hit {
			c.hit = true
			go c.over()
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (r *Runner) docker(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Docker, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// The run's input, checked before anything is made.
func check(job Job) (Language, error) {
	lang, ok := Lookup(job.Language)
	if !ok {
		return Language{}, fmt.Errorf("%w: %q", ErrUnknownLanguage, job.Language)
	}
	for name := range job.Files {
		if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
			return Language{}, fmt.Errorf("%w: %q", errFileName, name)
		}
	}
	return lang, job.Limits.validate()
}

// Program is a job's code made ready to run, once: written to a directory of its own and,
// for a compiled language, compiled in a container of its own (owner, 2026-11-26: compile
// once). Each Run is then a fresh container over the same files, with the program's
// own time limit from when it starts. Close removes what Prepare made.
type Program interface {
	// Run gives one run's result. A job that didn't compile gives its compile verdict
	// (CompileError, CompileTimeLimit) for every run, without running anything.
	Run(ctx context.Context, stdin string) (Result, error)
	Close()
}

type program struct {
	r    *Runner
	lang Language
	// The job's own limits, the toolchain's overhead not yet added.
	limits Limits
	// The job's directory: its files in code, and a compiled language's program in build.
	dir string
	// The compile's verdict when it didn't compile, for every run.
	failed *Result
}

func (p *program) code() string  { return filepath.Join(p.dir, "code") }
func (p *program) build() string { return filepath.Join(p.dir, "build") }

// Run is one job with one run: Prepare, then Run, then Close.
func (r *Runner) Run(ctx context.Context, job Job) (Result, error) {
	p, err := r.Prepare(ctx, job)
	if err != nil {
		return Result{}, err
	}
	defer p.Close()
	return p.Run(ctx, job.Stdin)
}

// Prepare writes a job's files and compiles them if its language compiles; job.Stdin
// isn't read. A failure to compile isn't an error: the Program gives its verdict.
func (r *Runner) Prepare(ctx context.Context, job Job) (Program, error) {
	lang, err := check(job)
	if err != nil {
		return nil, err
	}
	// What Docker gets, toolchain included, is held to the same bounds.
	if err := lang.with(job.Limits).validate(); err != nil {
		return nil, err
	}
	if lang.Compile != "" {
		if err := lang.compiling(job.Limits).validate(); err != nil {
			return nil, err
		}
	}
	dir, err := os.MkdirTemp(r.WorkDir, "job-")
	if err != nil {
		return nil, err
	}
	p := &program{r: r, lang: lang, limits: job.Limits, dir: dir}
	ready := false
	defer func() {
		if !ready {
			p.Close()
		}
	}()
	// Readable by the sandbox's user, which owns nothing here.
	if err := os.Chmod(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.Mkdir(p.code(), 0o755); err != nil {
		return nil, err
	}
	if err := os.Chmod(p.code(), 0o755); err != nil {
		return nil, err
	}
	for name, body := range job.Files {
		if err := os.WriteFile(filepath.Join(p.code(), name), []byte(body), 0o644); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if lang.Compile != "" {
		if err := p.compile(ctx); err != nil {
			return nil, err
		}
	}
	ready = true
	return p, nil
}

// compile runs the compiler in a container of its own, held to every rule a run is,
// with the compile's budget for its time. The program leaves the container only as a tar
// on its stdout, unpacked here into the job's build directory, which each run then
// mounts read-only: no container ever writes to the host.
func (p *program) compile(ctx context.Context) error {
	limits := p.lang.compiling(p.limits)
	// A tar of what fits in the container's /tmp, with room for the tar's own headers.
	out, err := p.r.container(ctx, Spec{Image: p.lang.Image, Cmd: p.lang.compileCmd(), RunDir: p.code(), Limits: limits},
		"", 2*limits.TmpMB<<20, false)
	if err != nil {
		return err
	}
	// Whatever the code makes the compiler do is the code's verdict; only a tar that
	// isn't plain files (errBuild) is the engine's fault.
	compileError := func(why string) error {
		p.failed = &Result{Verdict: CompileError, ExitCode: out.exit, Stderr: out.stderr + why}
		return nil
	}
	switch {
	case out.timedOut || out.took > time.Duration(limits.Seconds)*time.Second:
		p.failed = &Result{Verdict: CompileTimeLimit, ExitCode: out.exit, Stderr: out.stderr}
		return nil
	case out.exit != 0:
		// The compiler's own failure, or it ran out of memory: the code's, either way.
		return compileError("")
	case out.stdoutHit:
		return compileError("\nthe compiled program is too large\n")
	}
	if err := os.Mkdir(p.build(), 0o755); err != nil {
		return err
	}
	if err := os.Chmod(p.build(), 0o755); err != nil {
		return err
	}
	if err := unpack(p.build(), out.stdout); errors.Is(err, errTooMany) {
		return compileError(fmt.Sprintf("\nthe compiler made over %d files\n", maxBuildEntries))
	} else if err != nil {
		return err
	}
	return nil
}

func (p *program) Run(ctx context.Context, stdin string) (Result, error) {
	if p.failed != nil {
		return *p.failed, nil
	}
	limits := p.lang.with(p.limits)
	spec := Spec{Image: p.lang.Image, Cmd: p.lang.RunCmd(p.limits.MemoryMB), RunDir: p.code(), Limits: limits}
	if p.lang.Compile != "" {
		spec.BuildDir = p.build()
	}
	out, err := p.r.container(ctx, spec, stdin, limits.OutputKB*1024, true)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Verdict: verdict(outcome{
			output:   out.stdoutHit || out.stderrHit,
			timedOut: out.timedOut || out.took > time.Duration(limits.Seconds)*time.Second,
			oom:      out.oomKilled || (p.lang.OutOfMemory != "" && strings.Contains(out.stderr, p.lang.OutOfMemory)),
			exit:     out.exit,
		}),
		ExitCode: out.exit,
		Stdout:   out.stdout,
		Stderr:   out.stderr,
		TimeMs:   out.took.Milliseconds(),
	}, nil
}

func (p *program) Close() {
	if err := os.RemoveAll(p.dir); err != nil {
		fmt.Fprintln(os.Stderr, "exec: job dir not removed:", p.dir, err)
	}
}

// The most entries a compile may leave: a Java job's classes are a few dozen.
const maxBuildEntries = 4096

var (
	errBuild   = errors.New("the compiled program can't be unpacked")
	errTooMany = errors.New("the compile made too many files")
)

// unpack writes a compile's tar into dir: plain files and directories only, each inside
// dir (os.Root refuses any path out of it), owned here and readable by the sandbox's
// user, executable only if the compiler made it so.
func unpack(dir, tarball string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(strings.NewReader(tarball))
	for n := 0; ; n++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: %w", errBuild, err)
		}
		if n == maxBuildEntries {
			return errTooMany
		}
		name := path.Clean(h.Name)
		if name == "." {
			continue
		}
		if !filepath.IsLocal(name) {
			return fmt.Errorf("%w: %q is outside it", errBuild, h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return err
			}
			if err := root.Chmod(name, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if parent := path.Dir(name); parent != "." {
				if err := root.MkdirAll(parent, 0o755); err != nil {
					return err
				}
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if err := errors.Join(err, f.Close()); err != nil {
				return err
			}
			if err := root.Chmod(name, mode); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: %q is neither a file nor a directory", errBuild, h.Name)
		}
	}
}

// What one container did.
type exited struct {
	stdout, stderr       string
	stdoutHit, stderrHit bool
	timedOut, oomKilled  bool
	exit                 int
	// Its own running time, from the container's start to its exit.
	took time.Duration
}

// container makes, runs and always removes one container. Its time limit is
// spec.Limits.Seconds from the container's own start. stdout is kept to stdoutMax bytes
// and stderr to the spec's OutputKB; killOnOutput stops the container when either is
// passed (a run), or else they are only cut short (a compile, whose stderr is the
// compiler's).
func (r *Runner) container(ctx context.Context, spec Spec, stdin string, stdoutMax int, killOnOutput bool) (exited, error) {
	// Named before it exists, so it is removed even if create fails after the daemon
	// made it.
	spec.Name = "velox-run-" + randomHex()
	defer func() {
		if _, err := r.docker("rm", "-f", spec.Name); err != nil && !strings.Contains(err.Error(), "No such container") {
			fmt.Fprintln(os.Stderr, "exec: container not removed:", spec.Name, err)
		}
	}()
	if err := spec.Limits.validate(); err != nil {
		return exited{}, err
	}
	if _, err := r.docker(spec.createArgs(r.Runtime)...); err != nil {
		return exited{}, err
	}
	return r.start(ctx, spec.Name, stdin, time.Duration(spec.Limits.Seconds)*time.Second, stdoutMax, spec.Limits.OutputKB*1024, killOnOutput)
}

func (r *Runner) start(ctx context.Context, name, stdin string, limit time.Duration, stdoutMax, stderrMax int, killOnOutput bool) (exited, error) {
	cmd := exec.Command(r.Docker, "start", "-ai", name)
	var once sync.Once
	var timedOut atomic.Bool
	kill := func() {
		once.Do(func() {
			_, err := r.docker("kill", name)
			if err == nil {
				return
			}
			gone := strings.Contains(err.Error(), "is not running") || strings.Contains(err.Error(), "No such container")
			if !gone {
				fmt.Fprintln(os.Stderr, "exec: kill failed:", name, err)
			}
			// Not stopped by the daemon (or not started yet, which kill can't stop and
			// this Once won't try again): stop waiting on the attach; rm -f follows.
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		})
	}
	over := kill
	if !killOnOutput {
		over = func() {}
	}
	stdout := &capped{max: stdoutMax, over: over}
	stderr := &capped{max: stderrMax, over: over}
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := ctx.Err(); err != nil {
		return exited{}, err
	}
	if err := cmd.Start(); err != nil {
		return exited{}, err
	}
	expire := func() {
		timedOut.Store(true)
		kill()
	}
	// The backstop, if the start time can't be read.
	timer := time.AfterFunc(limit+startAllowance, expire)
	done := make(chan struct{})
	go r.deadline(name, limit, expire, done)
	stop := context.AfterFunc(ctx, kill)
	waitErr := cmd.Wait()
	close(done)
	timer.Stop()
	stop()
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return exited{}, waitErr
	}
	if err := ctx.Err(); err != nil {
		return exited{}, err
	}
	state, err := r.docker("inspect", "-f",
		"{{.State.Status}}|{{.State.OOMKilled}}|{{.State.ExitCode}}|{{.State.StartedAt}}|{{.State.FinishedAt}}|{{.State.Error}}",
		name)
	if err != nil {
		return exited{}, err
	}
	parts := strings.SplitN(state, "|", 6)
	if len(parts) != 6 || parts[0] != "exited" {
		// The CLI or the daemon failed, not the program: never a verdict.
		return exited{}, fmt.Errorf("%w: %s", errNotRun, state)
	}
	exit, err := strconv.Atoi(parts[2])
	if err != nil {
		return exited{}, fmt.Errorf("exit code %q: %w", parts[2], err)
	}
	began, err1 := time.Parse(time.RFC3339Nano, parts[3])
	ended, err2 := time.Parse(time.RFC3339Nano, parts[4])
	if err := errors.Join(err1, err2); err != nil {
		return exited{}, fmt.Errorf("container times: %w", err)
	}
	return exited{
		stdout:    stdout.buf.String(),
		stderr:    stderr.buf.String(),
		stdoutHit: stdout.hit,
		stderrHit: stderr.hit,
		timedOut:  timedOut.Load(),
		oomKilled: parts[1] == "true",
		exit:      exit,
		took:      ended.Sub(began),
	}, nil
}

// Watches for the container to start, then kills it when its own time is up: the
// limit runs from its start (the program's, or the compiler's), not from when it was
// asked for.
func (r *Runner) deadline(name string, limit time.Duration, expire func(), done <-chan struct{}) {
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-done:
			return
		case <-poll.C:
		}
		state, err := r.docker("inspect", "-f", "{{.State.Status}}|{{.State.StartedAt}}", name)
		if err != nil {
			// The backstop timer still holds; the run's own result reports the cause.
			return
		}
		status, at, _ := strings.Cut(state, "|")
		if status != "running" {
			if status == "created" {
				continue
			}
			return
		}
		began, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return
		}
		select {
		case <-done:
		case <-time.After(time.Until(began.Add(limit))):
			expire()
		}
		return
	}
}

type outcome struct {
	output, timedOut, oom bool
	exit                  int
}

// A run's one verdict, the first limit that applies: too much output, then too long,
// then too much memory, then the program's own failure. A compile's verdicts are its own
// (program.compile), so a slow compile is never the program's time limit.
func verdict(o outcome) Verdict {
	switch {
	case o.output:
		return OutputLimit
	case o.timedOut:
		return TimeLimit
	case o.oom:
		return MemoryLimit
	case o.exit != 0:
		return RuntimeError
	}
	return OK
}

func randomHex() string {
	b := make([]byte, 8)
	// crypto/rand.Read never fails on supported platforms (Go 1.24).
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
