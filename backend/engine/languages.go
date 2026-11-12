package engine

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// P5-W28-S03 (EXEC-2, C-04, Q-P5-04): the five launch languages. Each has its image,
// pinned by digest and at least two weeks old, its source file, how it compiles (once
// per submission, into /tmp) and how it runs. A toolchain's own needs (a JVM, a
// compiler) are added to the job's limits as its overhead, so a problem's limits stay
// about the program.
type Language struct {
	Image string
	// The file the submission is written to.
	Main string
	// Shell commands; Compile is empty for interpreted languages. %d in Run is the job's
	// memory in MB, so a runtime with its own heap sizes it to the job, not the
	// container.
	Compile, Run string
	Overhead     Overhead
	// What the runtime prints when its own heap is full: scored as a memory limit,
	// since the container's limit is never reached.
	OutOfMemory string
}

// A toolchain's own needs, added to the job's limits. Seconds is the compile's own
// budget: the program's clock starts when it has compiled. shortcut: the memory overhead
// stays available to the program after compiling (a cgroup's limit can't shrink inside
// the container); per-problem limits come from reference solutions run the same way
// (EXEC-2), so it is the same for everyone. Cap the run phase separately if references
// show it matters.
type Overhead struct {
	MemoryMB, Seconds, TmpMB int
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
	// The go build environment, set for the build only: the program gets none of it.
	goBuild = "cp -r /opt/gocache /tmp/gocache && GOCACHE=/tmp/gocache GOPATH=/tmp/gopath GOTOOLCHAIN=local CGO_ENABLED=0 go build -o /tmp/main /code/*.go"
)

var languages = map[string]Language{
	// The worker's own base (workers/jobs/Dockerfile), built 2026-10-21. -I and -S: no
	// user site, no PYTHON* variables, no current directory on the path.
	"python": {
		Image: images["python"].Image,
		Main:  "main.py",
		Run:   "python3 -I -S /code/main.py",
	},
	// The web's own base (apps/web/Dockerfile). TypeScript runs through Node 22's type
	// stripping, with no compiler.
	"javascript": {Image: images["javascript"].Image, Main: "main.js", Run: "node --max-old-space-size=%d /code/main.js",
		Overhead: Overhead{MemoryMB: 64}, OutOfMemory: "JavaScript heap out of memory"},
	"typescript": {Image: images["typescript"].Image, Main: "main.ts", Run: "node --max-old-space-size=%d /code/main.ts",
		Overhead: Overhead{MemoryMB: 64}, OutOfMemory: "JavaScript heap out of memory"},
	// Built 2026-10-26; the 21-jdk tag's current build was a day old.
	"java": {
		Image:       images["java"].Image,
		Main:        "Main.java",
		Compile:     "javac -d /tmp/out /code/Main.java",
		Run:         "java -Xmx%dm -XX:+UseSerialGC -cp /tmp/out Main",
		Overhead:    Overhead{MemoryMB: 256, Seconds: 6},
		OutOfMemory: "java.lang.OutOfMemoryError",
	},
	// Built 2026-06-24; the 14 tag's current build was a week old.
	"cpp": {
		Image:    images["cpp"].Image,
		Main:     "main.cpp",
		Compile:  "g++ -O2 -std=c++20 -o /tmp/main /code/main.cpp",
		Run:      "/tmp/main",
		Overhead: Overhead{MemoryMB: 256, Seconds: 5},
	},
	"go": {
		Image:    images["go"].Image,
		Main:     "main.go",
		Compile:  goBuild,
		Run:      "/tmp/main",
		Overhead: Overhead{MemoryMB: 384, Seconds: 8, TmpMB: 128},
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

// ready is printed once the code has compiled, before the program starts: a submission
// without it never compiled. readyShell is the same bytes for printf.
const (
	ready      = "\x1eREADY\n"
	readyShell = `\036READY\n`
)

// Cmd is the container's one command: compile (with no stdin, its output on stderr),
// say so, then become the program. memoryMB is the job's own memory, for the heap.
func (l Language) Cmd(memoryMB int) []string {
	run := l.Run
	if strings.Contains(run, "%d") {
		run = fmt.Sprintf(run, memoryMB)
	}
	if l.Compile == "" {
		return []string{"sh", "-c", "exec " + run}
	}
	return []string{"sh", "-c", l.Compile + " </dev/null >&2 || exit 1; printf '" + readyShell + "'; exec " + run}
}

// with adds the language's overhead to a job's limits.
func (l Language) with(j Limits) Limits {
	j.MemoryMB += l.Overhead.MemoryMB
	j.Seconds += l.Overhead.Seconds
	j.TmpMB += l.Overhead.TmpMB
	return j
}
