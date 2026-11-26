package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Owner, 2026-11-26 ("compile once + clear verdict"): a job compiles once, in a
// container of its own, and each run is a fresh container over the program it made.
// These drive the Runner against a fake docker (this test binary, run again as one), so
// the flow is checked on any machine; run_test.go has the real thing (EXEC_INTEGRATION).

func TestMain(m *testing.M) {
	if dir := os.Getenv("VELOX_FAKE_DOCKER"); dir != "" {
		os.Exit(fakeDocker(dir, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeDocker is just enough of the docker CLI for the Runner: create, start -ai,
// inspect, kill and rm, its state in files under dir. FAKE_COMPILE says what a compile
// does (ok, error, slow, sleepy: ok after 1.5 s, symlink, many: too many files, stall:
// never starts, and can't be killed); FAKE_RUN what a run does
// (echo: its stdin and the compiled program's bytes back; slow).
func fakeDocker(dir string, args []string) int {
	logf := func(format string, a ...any) {
		f, err := os.OpenFile(filepath.Join(dir, "log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, format+"\n", a...)
			_ = f.Close()
		}
	}
	file := func(name, ext string) string { return filepath.Join(dir, name+"."+ext) }
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	write := func(p, s string) { _ = os.WriteFile(p, []byte(s), 0o644) }
	isCompile := func(cmd string) bool { return strings.Contains(cmd, "tar -cf -") }
	name := args[len(args)-1]
	switch args[0] {
	case "create":
		name = args[slices.Index(args, "--name")+1]
		write(file(name, "create"), strings.Join(args, "\x00"))
		kind := "run"
		if isCompile(args[len(args)-1]) {
			kind = "compile"
		}
		logf("create %s %s", name, kind)
		return 0
	case "start":
		create := strings.Split(read(file(name, "create")), "\x00")
		write(file(name, "started"), time.Now().UTC().Format(time.RFC3339Nano))
		finish := func(code int) int {
			write(file(name, "finished"), fmt.Sprintf("%s|%d", time.Now().UTC().Format(time.RFC3339Nano), code))
			return code
		}
		waitKilled := func() int {
			for !exists(file(name, "killed")) {
				time.Sleep(20 * time.Millisecond)
			}
			return finish(137)
		}
		if isCompile(create[len(create)-1]) {
			logf("compile %s", name)
			behaviour := os.Getenv("FAKE_COMPILE")
			switch behaviour {
			case "error":
				fmt.Fprint(os.Stderr, "/code/main.cpp:3:1: error: nope\n")
				return finish(1)
			case "slow":
				return waitKilled()
			case "sleepy":
				time.Sleep(1500 * time.Millisecond)
			case "stall":
				// The daemon never gets it going: it stays created.
				_ = os.Remove(file(name, "started"))
				time.Sleep(time.Hour)
			}
			tw := tar.NewWriter(os.Stdout)
			_ = tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755})
			body := "the program"
			_ = tw.WriteHeader(&tar.Header{Name: "./main", Typeflag: tar.TypeReg, Mode: 0o700, Size: int64(len(body))})
			_, _ = tw.Write([]byte(body))
			_ = tw.WriteHeader(&tar.Header{Name: "./pkg/A.class", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1})
			_, _ = tw.Write([]byte("A"))
			for i := range maxBuildEntries {
				if behaviour == "many" {
					_ = tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("./pkg/C%d.class", i), Typeflag: tar.TypeReg, Mode: 0o644})
				}
			}
			if behaviour == "symlink" {
				_ = tw.WriteHeader(&tar.Header{Name: "./escape", Typeflag: tar.TypeSymlink, Linkname: "/etc"})
			}
			_ = tw.Close()
			return finish(0)
		}
		logf("run %s", name)
		if os.Getenv("FAKE_RUN") == "slow" {
			return waitKilled()
		}
		stdin, _ := io.ReadAll(os.Stdin)
		program := "none"
		for i, a := range create {
			if a == "-v" && strings.HasSuffix(create[i+1], ":/build:ro") {
				program = read(filepath.Join(strings.TrimSuffix(create[i+1], ":/build:ro"), "main"))
			}
		}
		fmt.Printf("ran:%s:%s", stdin, program)
		return finish(0)
	case "inspect":
		format := args[2]
		started := read(file(name, "started"))
		finished, code, done := strings.Cut(read(file(name, "finished")), "|")
		if strings.Contains(format, "OOMKilled") {
			if !done {
				fmt.Printf("running|false|0|%s|0001-01-01T00:00:00Z|\n", started)
				return 0
			}
			fmt.Printf("exited|false|%s|%s|%s|\n", code, started, finished)
			return 0
		}
		switch {
		case done:
			fmt.Printf("exited|%s\n", started)
		case started != "":
			fmt.Printf("running|%s\n", started)
		default:
			fmt.Println("created|0001-01-01T00:00:00Z")
		}
		return 0
	case "kill":
		if !exists(file(name, "started")) {
			fmt.Fprintf(os.Stderr, "Error response from daemon: cannot kill container: %s: container %s is not running\n", name, name)
			return 1
		}
		write(file(name, "killed"), "")
		return 0
	case "rm":
		if !exists(file(name, "create")) {
			fmt.Fprintln(os.Stderr, "Error: No such container:", name)
			return 1
		}
		logf("rm %s", name)
		return 0
	}
	fmt.Fprintln(os.Stderr, "fake docker: unknown command", args)
	return 2
}

type fakeRun struct {
	r     *Runner
	state string
}

func fake(t *testing.T, compile, run string) fakeRun {
	t.Helper()
	state := t.TempDir()
	t.Setenv("VELOX_FAKE_DOCKER", state)
	t.Setenv("FAKE_COMPILE", compile)
	t.Setenv("FAKE_RUN", run)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return fakeRun{r: &Runner{WorkDir: t.TempDir(), Docker: exe}, state: state}
}

// The log's lines, each its words: create <name> <kind>, compile <name>, run <name>,
// rm <name>.
func (f fakeRun) log(t *testing.T) [][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.state, "log"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var out [][]string
	for l := range strings.Lines(string(raw)) {
		out = append(out, strings.Fields(l))
	}
	return out
}

func (f fakeRun) count(t *testing.T, what string) int {
	t.Helper()
	n := 0
	for _, l := range f.log(t) {
		if l[0] == what {
			n++
		}
	}
	return n
}

// Every container made, removed; nothing of the job left in the work dir.
func (f fakeRun) clean(t *testing.T) {
	t.Helper()
	created, removed := map[string]bool{}, map[string]bool{}
	for _, l := range f.log(t) {
		switch l[0] {
		case "create":
			created[l[1]] = true
		case "rm":
			removed[l[1]] = true
		}
	}
	for name := range created {
		if !removed[name] {
			t.Errorf("container %s not removed", name)
		}
	}
	entries, err := os.ReadDir(f.r.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("job dirs left: %d", len(entries))
	}
}

// Each container's create arguments, by "<kind> <name>".
func (f fakeRun) creates(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, l := range f.log(t) {
		if l[0] == "create" {
			raw, err := os.ReadFile(filepath.Join(f.state, l[1]+".create"))
			if err != nil {
				t.Fatal(err)
			}
			out[l[2]+" "+l[1]] = strings.Split(string(raw), "\x00")
		}
	}
	return out
}

var cppJob = Job{Language: "cpp", Files: map[string]string{"main.cpp": "int main() {}"},
	Limits: Limits{MemoryMB: 64, CPUs: 1, Pids: 32, TmpMB: 8, Seconds: 1, OutputKB: 64}}

func TestACompiledJobCompilesOnceAndEveryRunUsesItsProgram(t *testing.T) {
	f := fake(t, "ok", "echo")
	p, err := f.r.Prepare(t.Context(), cppJob)
	if err != nil {
		t.Fatal(err)
	}
	for _, stdin := range []string{"a", "b", "c"} {
		got, err := p.Run(t.Context(), stdin)
		if err != nil {
			t.Fatal(err)
		}
		if got.Verdict != OK || got.Stdout != "ran:"+stdin+":the program" {
			t.Fatalf("%s: %+v", stdin, got)
		}
	}
	p.Close()
	if n := f.count(t, "compile"); n != 1 {
		t.Errorf("compiled %d times", n)
	}
	if n := f.count(t, "run"); n != 3 {
		t.Errorf("ran %d times", n)
	}
	f.clean(t)
}

// The compile's container and every run's hold the same rules (EXEC-3); each mount is
// the job's own and read-only, and only a run of a compiled program gets the program's.
func TestTheCompileAndEveryRunAreIsolatedAlike(t *testing.T) {
	f := fake(t, "ok", "echo")
	f.r.Runtime = "runsc"
	if _, err := f.r.Run(t.Context(), cppJob); err != nil {
		t.Fatal(err)
	}
	creates := f.creates(t)
	if len(creates) != 2 {
		t.Fatalf("%d containers", len(creates))
	}
	for kind, args := range creates {
		has(t, args, "--network", "none")
		has(t, args, "--read-only")
		has(t, args, "--tmpfs", "/tmp:rw,exec,nosuid,nodev,size=8m")
		has(t, args, "--user", "65534:65534")
		has(t, args, "--cap-drop", "ALL")
		has(t, args, "--security-opt", "no-new-privileges")
		has(t, args, "--pids-limit", "32")
		// The job's 64 MB and g++'s 256, for the compile and the program alike.
		has(t, args, "--memory", "320m")
		has(t, args, "--memory-swap", "320m")
		has(t, args, "--cpus", "1")
		has(t, args, "--env-file", "/dev/null")
		has(t, args, "--log-driver", "none")
		has(t, args, "--runtime", "runsc")
		has(t, args, "--label", "velox.run=1")
		var mounts []string
		for i, a := range args {
			if a == "-v" || a == "--volume" || a == "--mount" {
				mounts = append(mounts, args[i+1])
			}
			if a == "--privileged" || strings.HasPrefix(a, "--cap-add") || a == "-e" || a == "--env" {
				t.Errorf("%s: %q", kind, a)
			}
		}
		for _, m := range mounts {
			if !strings.HasSuffix(m, ":ro") || !strings.HasPrefix(m, f.r.WorkDir+"/job-") {
				t.Errorf("%s: mount %q", kind, m)
			}
		}
		want := []string{"/code:/code:ro"}
		if strings.HasPrefix(kind, "run") {
			want = append(want, "/build:/build:ro")
		}
		if len(mounts) != len(want) {
			t.Fatalf("%s: mounts %q", kind, mounts)
		}
		for i, w := range want {
			if !strings.HasSuffix(mounts[i], w) {
				t.Errorf("%s: mount %q, want …%s", kind, mounts[i], w)
			}
		}
	}
}

func TestCodeThatDoesntCompileGivesEveryRunCompileErrorWithoutRunning(t *testing.T) {
	f := fake(t, "error", "echo")
	p, err := f.r.Prepare(t.Context(), cppJob)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		got, err := p.Run(t.Context(), "x")
		if err != nil || got.Verdict != CompileError || !strings.Contains(got.Stderr, "error: nope") {
			t.Fatalf("%v %+v", err, got)
		}
	}
	p.Close()
	if n := f.count(t, "run"); n != 0 {
		t.Errorf("ran %d times", n)
	}
	f.clean(t)
}

// A compile that overruns its own budget is CompileTimeLimit, never the program's
// TimeLimit, and nothing runs.
func TestACompileThatOverrunsItsBudgetIsCompileTimeLimit(t *testing.T) {
	budget(t, "cpp", 1)
	f := fake(t, "slow", "echo")
	start := time.Now()
	p, err := f.r.Prepare(t.Context(), cppJob)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < time.Second || took > 5*time.Second {
		t.Errorf("killed after %s, budget 1 s", took)
	}
	for range 2 {
		got, err := p.Run(t.Context(), "x")
		if err != nil || got.Verdict != CompileTimeLimit {
			t.Fatalf("%v %+v", err, got)
		}
	}
	p.Close()
	if n := f.count(t, "run"); n != 0 {
		t.Errorf("ran %d times", n)
	}
	f.clean(t)
}

// The compile has its own budget and the program its own limit, from its own start: a
// compile longer than the program's limit, within its budget, costs the program nothing.
func TestTheProgramsLimitStartsWhenItDoes(t *testing.T) {
	budget(t, "cpp", 3)
	f := fake(t, "sleepy", "echo")
	got, err := f.r.Run(t.Context(), cppJob)
	if err != nil || got.Verdict != OK {
		t.Fatalf("%v %+v", err, got)
	}
	if got.TimeMs > 1000 {
		t.Errorf("the compile was counted: %d ms", got.TimeMs)
	}
	f.clean(t)
}

func TestARunThatOverrunsIsTimeLimit(t *testing.T) {
	f := fake(t, "ok", "slow")
	got, err := f.r.Run(t.Context(), cppJob)
	if err != nil || got.Verdict != TimeLimit {
		t.Fatalf("%v %+v", err, got)
	}
	f.clean(t)
}

// An interpreted language has no compile: each run is the program.
func TestAnInterpretedJobHasNoCompile(t *testing.T) {
	f := fake(t, "ok", "echo")
	p, err := f.r.Prepare(t.Context(), Job{Language: "python", Files: map[string]string{"main.py": "x"},
		Limits: cppJob.Limits})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Run(t.Context(), "a")
	if err != nil || got.Stdout != "ran:a:none" {
		t.Fatalf("%v %+v", err, got)
	}
	p.Close()
	if f.count(t, "compile") != 0 {
		t.Error("compiled")
	}
	for kind, args := range f.creates(t) {
		if slices.ContainsFunc(args, func(a string) bool { return strings.HasSuffix(a, ":/build:ro") }) {
			t.Errorf("%s: a program mount", kind)
		}
	}
	f.clean(t)
}

// A compile's output that isn't plain files and directories is the engine's fault: the
// job fails, and nothing is left.
func TestAnUnsafeBuildIsRefusedAndCleanedUp(t *testing.T) {
	f := fake(t, "symlink", "echo")
	if _, err := f.r.Prepare(t.Context(), cppJob); !errors.Is(err, errBuild) {
		t.Fatalf("got %v", err)
	}
	f.clean(t)
}

// Thousands of classes are the code's doing, not the engine's: a compile error.
func TestACompileThatMakesTooManyFilesIsACompileError(t *testing.T) {
	f := fake(t, "many", "echo")
	got, err := f.r.Run(t.Context(), cppJob)
	if err != nil || got.Verdict != CompileError || !strings.Contains(got.Stderr, "files") {
		t.Fatalf("%v %+v", err, got)
	}
	f.clean(t)
}

// A container that never started can't be killed by the daemon; the attach is stopped
// instead, so a cancelled job never hangs.
func TestACancelledCompileThatNeverStartedStillStops(t *testing.T) {
	f := fake(t, "stall", "echo")
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := f.r.Prepare(ctx, cppJob)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("no error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("hung")
	}
	f.clean(t)
}

func TestACancelledPrepareLeavesNothing(t *testing.T) {
	f := fake(t, "slow", "echo")
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if _, err := f.r.Prepare(ctx, cppJob); err == nil {
		t.Fatal("no error")
	}
	f.clean(t)
}

// budget sets a language's compile budget for one test. It changes the package's table:
// no test that uses it may run in parallel.
func budget(t *testing.T, language string, seconds int) {
	t.Helper()
	l := languages[language]
	was := l.CompileSeconds
	l.CompileSeconds = seconds
	languages[language] = l
	t.Cleanup(func() {
		l.CompileSeconds = was
		languages[language] = l
	})
}

// entry is one tar entry; body only for a regular file.
type entry struct {
	h    tar.Header
	body string
}

func tarOf(t *testing.T, entries ...entry) string {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range entries {
		e.h.Size = int64(len(e.body))
		if err := tw.WriteHeader(&e.h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func reg(name, body string, mode int64) entry {
	return entry{tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode}, body}
}

func TestUnpackKeepsOnlyPlainFilesInsideItsDir(t *testing.T) {
	dir := t.TempDir()
	if err := unpack(dir, tarOf(t, entry{h: tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o700}},
		reg("./main", "exe", 0o700), reg("./p/q/A.class", "A", 0o600))); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{"main": 0o755, "p": 0o755 | os.ModeDir, "p/q/A.class": 0o644} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode() != mode {
			t.Errorf("%s: %s, want %s", name, info.Mode(), mode)
		}
	}
	outside := filepath.Join(filepath.Dir(dir), "escaped")
	for name, entries := range map[string][]entry{
		"parent":   {reg("../escaped", "x", 0o644)},
		"absolute": {reg(outside, "x", 0o644)},
		"symlink":  {{h: tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc"}}},
		"hardlink": {{h: tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "/etc/passwd"}}},
		"device":   {{h: tar.Header{Name: "dev", Typeflag: tar.TypeChar}}},
		"twice":    {reg("a", "x", 0o644), reg("a", "y", 0o644)},
	} {
		if err := unpack(t.TempDir(), tarOf(t, entries...)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("a file was written outside")
	}
	many := make([]entry, maxBuildEntries+1)
	for i := range many {
		many[i] = reg(fmt.Sprint(i), "", 0o644)
	}
	if err := unpack(t.TempDir(), tarOf(t, many...)); !errors.Is(err, errTooMany) {
		t.Errorf("too many: %v", err)
	}
}
