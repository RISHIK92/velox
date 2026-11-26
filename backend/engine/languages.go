package engine

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// P5-W28-S03 (EXEC-2, C-04, Q-P5-04): the five launch languages. Each has its image,
// pinned by digest and at least two weeks old, its source file, how it compiles and how
// it runs. A compiled language compiles once per job, in a container of its own, into
// compileOut; every run of the job then finds that program read-only at buildDir. A
// toolchain's own needs (a JVM, a compiler) are added to the job's limits as its
// overhead, so a problem's limits stay about the program.
type Language struct {
	Image string
	// The file the submission is written to.
	Main string
	// Shell commands; Compile is empty for interpreted languages. Compile writes into
	// compileOut; Run reads the program from buildDir. %d in Run is the job's memory in
	// MB, so a runtime with its own heap sizes it to the job, not the container.
	Compile, Run string
	// The compile's own wall-clock budget (owner, 2026-11-26): it has a container of its
	// own, so the program's time limit starts only when the program does, and a compile
	// that overruns it is CompileTimeLimit, never the program's TimeLimit.
	CompileSeconds int
	Overhead       Overhead
	// What the runtime prints when its own heap is full: scored as a memory limit,
	// since the container's limit is never reached.
	OutOfMemory string
}

// A toolchain's own needs, added to the job's limits, the compile's and every run's
// alike: the program keeps the memory a compiled language's toolchain had when both ran
// in one container, so per-problem limits, which come from reference solutions run the
// same way (EXEC-2), mean what they meant.
type Overhead struct {
	MemoryMB, TmpMB int
}

// P5-W28-S05: the images, in one file the infra reads too (it mirrors them into ECR
// for the isolated execution hosts, EXEC-5). Each is pinned by digest and at least two
// weeks old; Go's is built here (images/go).
//
//go:embed images.json
var imagesJSON []byte

type imageEntry struct {
	Image string `json:"image"`
	Build string `json:"build,omitempty"`
}

var images = func() map[string]imageEntry {
	var m map[string]imageEntry
	if err := json.Unmarshal(imagesJSON, &m); err != nil {
		// A build-time file: a broken one fails every test and the binary at start.
		panic("engine/images.json: " + err.Error())
	}
	return m
}()

const (
	// Where a compile leaves the program, in its own container's /tmp.
	compileOut = "/tmp/out"
	// Where each run of the job finds it, mounted read-only.
	buildDir = "/build"
	// The go build environment, set for the build only: the program gets none of it.
	goBuild = "cp -r /opt/gocache /tmp/gocache && GOCACHE=/tmp/gocache GOPATH=/tmp/gopath GOTOOLCHAIN=local CGO_ENABLED=0 go build -o /tmp/out/main /code/*.go"
)

var languages = map[string]Language{
	// Interval's Python worker base, built 2026-10-21. -I and -S: no
	// user site, no PYTHON* variables, no current directory on the path.
	"python": {
		Image: images["python"].Image,
		Main:  "main.py",
		Run:   "python3 -I -S /code/main.py",
		// A large allocation can be refused before the cgroup's OOM killer acts: Python
		// then raises MemoryError and exits 1, which is still the memory limit.
		OutOfMemory: "MemoryError",
	},
	// Interval's web base. TypeScript runs through Node 22's type
	// stripping, with no compiler.
	"javascript": {Image: images["javascript"].Image, Main: "main.js", Run: "node --max-old-space-size=%d /code/main.js",
		Overhead: Overhead{MemoryMB: 64}, OutOfMemory: "JavaScript heap out of memory"},
	"typescript": {Image: images["typescript"].Image, Main: "main.ts", Run: "node --max-old-space-size=%d /code/main.ts",
		Overhead: Overhead{MemoryMB: 64}, OutOfMemory: "JavaScript heap out of memory"},
	// Built 2026-10-26; the 21-jdk tag's current build was a day old. Compile budgets
	// (owner, 2026-11-26): three times what an LLD harness alone takes on one CPU, so a
	// heavy but fair submission (a C++ <regex>, many Java classes) still compiles.
	"java": {
		Image:          images["java"].Image,
		Main:           "Main.java",
		Compile:        "javac -d /tmp/out /code/Main.java",
		Run:            "java -Xmx%dm -XX:+UseSerialGC -cp /build Main",
		CompileSeconds: 15,
		Overhead:       Overhead{MemoryMB: 256},
		OutOfMemory:    "java.lang.OutOfMemoryError",
	},
	// Built 2026-06-24; the 14 tag's current build was a week old.
	"cpp": {
		Image:          images["cpp"].Image,
		Main:           "main.cpp",
		Compile:        "g++ -O2 -std=c++20 -o /tmp/out/main /code/main.cpp",
		Run:            "/build/main",
		CompileSeconds: 15,
		Overhead:       Overhead{MemoryMB: 256},
	},
	"go": {
		Image:          images["go"].Image,
		Main:           "main.go",
		Compile:        goBuild,
		Run:            "/build/main",
		CompileSeconds: 20,
		Overhead:       Overhead{MemoryMB: 384, TmpMB: 128},
	},
}

// Lookup gives a language by name. EXEC_IMAGE_<NAME> (EXEC_IMAGE_CPP, …) replaces its
// image: the execution hosts run the copies mirrored into ECR.
func Lookup(name string) (Language, bool) {
	l, ok := languages[name]
	if image := os.Getenv("EXEC_IMAGE_" + strings.ToUpper(name)); ok && image != "" {
		l.Image = image
	}
	return l, ok
}

// Names lists the languages, for the suite and the CLI (a test keeps it whole).
func Names() []string {
	return []string{"python", "javascript", "typescript", "java", "cpp", "go"}
}

// compileCmd is the compile container's one command: compile (with no stdin, its output
// on stderr), then send what it made as a tar on stdout, the only way out of the
// container. Exit 1 is the code's own failure to compile.
func (l Language) compileCmd() []string {
	return []string{"sh", "-c", "{ mkdir -p " + compileOut + " && " + l.Compile + "; } </dev/null >&2 || exit 1; cd " + compileOut + " && exec tar -cf - ."}
}

// RunCmd is a run's one command: the program. memoryMB is the job's own memory, for the
// heap.
func (l Language) RunCmd(memoryMB int) []string {
	run := l.Run
	if strings.Contains(run, "%d") {
		run = fmt.Sprintf(run, memoryMB)
	}
	return []string{"sh", "-c", "exec " + run}
}

// with adds the language's overhead to a job's limits.
func (l Language) with(j Limits) Limits {
	j.MemoryMB += l.Overhead.MemoryMB
	j.TmpMB += l.Overhead.TmpMB
	return j
}

// compiling is the compile container's limits: the job's, with the toolchain's overhead,
// and the compile's own budget for its time.
func (l Language) compiling(j Limits) Limits {
	j = l.with(j)
	j.Seconds = l.CompileSeconds
	return j
}
