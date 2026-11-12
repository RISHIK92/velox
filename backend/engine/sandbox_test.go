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
	args := spec().createArgs("")
	mounts := 0
	for i, a := range args {
		if a == "-v" || a == "--volume" || a == "--mount" {
			mounts++
			if args[i+1] != "/work/run-1:/code:ro" {
				t.Errorf("mount %q", args[i+1])
			}
		}
		if a == "-e" || a == "--env" || strings.HasPrefix(a, "--env=") {
			t.Errorf("an environment variable is passed: %q", args)
		}
		if a == "--privileged" || strings.HasPrefix(a, "--cap-add") {
			t.Errorf("privilege added: %q", a)
		}
	}
	if mounts != 1 {
		t.Errorf("want exactly one mount, got %d", mounts)
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
		{outcome{timedOut: true, notCompiled: true, oom: true, exit: 137}, TimeLimit},
		{outcome{notCompiled: true, oom: true, exit: 1}, CompileError},
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

func TestEveryLanguageIsListedAndCompiledOnesSayReady(t *testing.T) {
	if len(Names()) != len(languages) {
		t.Fatalf("Names has %d, the table %d", len(Names()), len(languages))
	}
	for _, name := range Names() {
		l, ok := Lookup(name)
		if !ok {
			t.Fatalf("%s isn't in the table", name)
		}
		script := l.Cmd(200)[2]
		if l.Compile != "" && !strings.Contains(script, "printf '"+readyShell+"'") {
			t.Errorf("%s: no READY: %s", name, script)
		}
		if l.Compile != "" && !strings.Contains(script, "</dev/null") {
			t.Errorf("%s: the compiler can read the job's stdin", name)
		}
		if strings.Contains(script, "%!") || strings.Contains(script, "%d") {
			t.Errorf("%s: unformatted: %s", name, script)
		}
	}
	if ready != "\x1eREADY\n" || readyShell != `\036READY\n` {
		t.Error("the marker's two spellings differ")
	}
	java, _ := Lookup("java")
	if !strings.Contains(java.Cmd(200)[2], "-Xmx200m") {
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
