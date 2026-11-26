package engine

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// P5-W28-S01 (EXEC-3, Q-P5-02): every run is a fresh container holding every rule.
func spec() Spec {
	return Spec{
		Name:   "velox-run-abc",
		Image:  "img@sha256:abc",
		Cmd:    []string{"python3", "/code/main.py"},
		RunDir: "/work/run-1",
		Limits: Limits{MemoryMB: 128, CPUs: 1, Pids: 64, TmpMB: 16},
	}
}

func has(t *testing.T, args []string, want ...string) {
	t.Helper()
	for i := range args {
		if slices.Equal(args[i:min(i+len(want), len(args))], want) {
			return
		}
	}
	t.Errorf("missing %q in %q", want, args)
}

func TestEveryIsolationRuleIsOnTheContainer(t *testing.T) {
	args := spec().createArgs("")
	has(t, args, "--network", "none")
	has(t, args, "--read-only")
	has(t, args, "--tmpfs", "/tmp:rw,exec,nosuid,nodev,size=16m")
	has(t, args, "--user", "65534:65534")
	has(t, args, "--cap-drop", "ALL")
	has(t, args, "--security-opt", "no-new-privileges")
	has(t, args, "--pids-limit", "64")
	has(t, args, "--memory", "128m")
	// No swap past the memory limit.
	has(t, args, "--memory-swap", "128m")
	has(t, args, "--cpus", "1")
	has(t, args, "--env-file", "/dev/null")
	has(t, args, "--label", "velox.run=1")
	has(t, args, "--name", "velox-run-abc")
	has(t, args, "--log-driver", "none")
	has(t, args, "--entrypoint", "")
	has(t, args, "--shm-size", "16m")
	has(t, args, "--ulimit", "nofile=256:256")
	has(t, args, "--ulimit", "fsize=16777216:16777216")
	// Docker's default seccomp profile and AppArmor stay on: nothing overrides them.
	for i, a := range args {
		if a == "--security-opt" && args[i+1] != "no-new-privileges" {
			t.Errorf("security option %q", args[i+1])
		}
	}
	has(t, args, "-i")
	// The command comes last, after the image.
	if !slices.Equal(args[len(args)-3:], []string{"img@sha256:abc", "python3", "/code/main.py"}) {
		t.Errorf("image and command not last: %q", args)
	}
}

func TestOnlyItsOwnRunDirIsMountedReadOnlyAndNoEnvironmentPasses(t *testing.T) {
	built := spec()
	built.BuildDir = "/work/job-1/build"
	for name, c := range map[string]struct {
		spec   Spec
		mounts []string
	}{
		"interpreted, or a compile": {spec(), []string{"/work/run-1:/code:ro"}},
		"a compiled program's run":  {built, []string{"/work/run-1:/code:ro", "/work/job-1/build:/build:ro"}},
	} {
		args := c.spec.createArgs("")
		var mounts []string
		for i, a := range args {
			if a == "-v" || a == "--volume" || a == "--mount" {
				mounts = append(mounts, args[i+1])
			}
			if a == "-e" || a == "--env" || strings.HasPrefix(a, "--env=") {
				t.Errorf("%s: an environment variable is passed: %q", name, args)
			}
			if a == "--privileged" || strings.HasPrefix(a, "--cap-add") {
				t.Errorf("%s: privilege added: %q", name, a)
			}
		}
		if !slices.Equal(mounts, c.mounts) {
			t.Errorf("%s: mounts %q, want %q", name, mounts, c.mounts)
		}
	}
}

func TestEveryLimitMustBeSetAndBounded(t *testing.T) {
	ok := spec().Limits
	ok.Seconds, ok.OutputKB = 3, 64
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Limits){
		"no memory":  func(l *Limits) { l.MemoryMB = 0 },
		"no pids":    func(l *Limits) { l.Pids = 0 },
		"no cpus":    func(l *Limits) { l.CPUs = 0 },
		"no tmp":     func(l *Limits) { l.TmpMB = 0 },
		"no seconds": func(l *Limits) { l.Seconds = 0 },
		"no output":  func(l *Limits) { l.OutputKB = 0 },
		"too much":   func(l *Limits) { l.MemoryMB = 1 << 20 },
		"too long":   func(l *Limits) { l.Seconds = 3600 },
	} {
		l := ok
		change(&l)
		if err := l.validate(); err == nil {
			t.Errorf("%s: accepted %+v", name, l)
		}
	}
}

func TestTheRuntimeIsNamedOnlyWhenSet(t *testing.T) {
	if slices.Contains(spec().createArgs(""), "--runtime") {
		t.Error("runtime set without EXEC_RUNTIME")
	}
	has(t, spec().createArgs("runsc"), "--runtime", "runsc")
}

func TestBadInputIsRefusedBeforeAnythingIsMade(t *testing.T) {
	good := Limits{MemoryMB: 64, CPUs: 1, Pids: 32, TmpMB: 8, Seconds: 3, OutputKB: 64}
	// A docker that doesn't exist: reaching it would be a different error.
	r := &Runner{WorkDir: t.TempDir(), Docker: "/nonexistent/docker"}
	for _, name := range []string{"../x", "a/b", "", ".", ".."} {
		_, err := r.Run(t.Context(), Job{Language: "python", Files: map[string]string{name: "x"}, Limits: good})
		if !errors.Is(err, errFileName) {
			t.Errorf("%q: %v", name, err)
		}
	}
	if _, err := r.Run(t.Context(), Job{Language: "cobol", Limits: good}); !errors.Is(err, ErrUnknownLanguage) {
		t.Error(err)
	}
	if _, err := r.Run(t.Context(), Job{Language: "python", Limits: Limits{}}); !errors.Is(err, errLimits) {
		t.Error(err)
	}
}

func TestTheFirstLimitHitIsTheVerdict(t *testing.T) {
	cases := []struct {
		o    outcome
		want Verdict
	}{
		{outcome{output: true, timedOut: true, oom: true, exit: 137}, OutputLimit},
		{outcome{timedOut: true, oom: true, exit: 137}, TimeLimit},
		{outcome{oom: true, exit: 137}, MemoryLimit},
		{outcome{exit: 1}, RuntimeError},
		{outcome{}, OK},
	}
	for _, c := range cases {
		if got := verdict(c.o); got != c.want {
			t.Errorf("%+v: %s, want %s", c.o, got, c.want)
		}
	}
}

// Owner, 2026-11-26: a compiled language compiles in a container of its own, with its
// own budget, and sends its program out as a tar; each run reads it from /build.
func TestEveryLanguageIsListedAndCompiledOnesCompileApart(t *testing.T) {
	if len(Names()) != len(languages) {
		t.Fatalf("Names has %d, the table %d", len(Names()), len(languages))
	}
	budgets := map[string]int{"python": 0, "javascript": 0, "typescript": 0, "java": 15, "cpp": 15, "go": 20}
	for _, name := range Names() {
		l, ok := Lookup(name)
		if !ok {
			t.Fatalf("%s isn't in the table", name)
		}
		if l.CompileSeconds != budgets[name] {
			t.Errorf("%s: a compile budget of %d s, want %d", name, l.CompileSeconds, budgets[name])
		}
		run := l.RunCmd(200)[2]
		if strings.Contains(run, "%!") || strings.Contains(run, "%d") || strings.Contains(run, "/tmp/") {
			t.Errorf("%s: run %s", name, run)
		}
		if l.Compile == "" {
			if strings.Contains(run, buildDir) {
				t.Errorf("%s: reads a program it never compiled: %s", name, run)
			}
			continue
		}
		script := l.compileCmd()[2]
		if !strings.Contains(script, "</dev/null") {
			t.Errorf("%s: the compiler can read the job's stdin", name)
		}
		// Only the tar reaches stdout: the compiler's own output goes to stderr.
		if !strings.Contains(script, "} </dev/null >&2 || exit 1; cd "+compileOut+" && exec tar -cf - .") {
			t.Errorf("%s: compile %s", name, script)
		}
		if !strings.Contains(l.Compile, compileOut) || !strings.Contains(run, buildDir) {
			t.Errorf("%s: compiles to %s, runs %s", name, l.Compile, run)
		}
		if c := l.compiling(Limits{MemoryMB: 64, TmpMB: 8, Seconds: 2}); c.Seconds != l.CompileSeconds || c.MemoryMB != 64+l.Overhead.MemoryMB {
			t.Errorf("%s: compiles within %+v", name, c)
		}
		if r := l.with(Limits{MemoryMB: 64, TmpMB: 8, Seconds: 2}); r.Seconds != 2 {
			t.Errorf("%s: a run's limit isn't the job's: %+v", name, r)
		}
	}
	java, _ := Lookup("java")
	if !strings.Contains(java.RunCmd(200)[2], "-Xmx200m") {
		t.Error("the JVM's heap isn't the job's memory")
	}
}

// P5-W28-S05: one list of images for the engine and the infra, each pinned.
func TestEveryLanguageHasAPinnedImageThatCanBeReplaced(t *testing.T) {
	for _, name := range Names() {
		entry, ok := images[name]
		if !ok {
			t.Fatalf("%s has no image", name)
		}
		pinned := strings.Contains(entry.Image, "@sha256:")
		if !pinned && entry.Build == "" {
			t.Errorf("%s: %s is neither pinned nor built here", name, entry.Image)
		}
		if l, _ := Lookup(name); l.Image != entry.Image {
			t.Errorf("%s: the table's image isn't the file's", name)
		}
	}
	// No image the engine wouldn't use (the infra mirrors and pre-pulls every entry).
	if len(images) != len(Names()) {
		t.Errorf("images.json has %d entries for %d languages", len(images), len(Names()))
	}
	t.Setenv("EXEC_IMAGE_CPP", "123.dkr.ecr.ap-south-1.amazonaws.com/gcc@sha256:abc")
	if l, _ := Lookup("cpp"); l.Image != "123.dkr.ecr.ap-south-1.amazonaws.com/gcc@sha256:abc" {
		t.Errorf("override ignored: %s", l.Image)
	}
}
