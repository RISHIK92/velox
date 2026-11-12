package engine

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// P5-W28-S01 (EXEC-3): real runs in Docker. Set EXEC_INTEGRATION=1 to run them (CI's
// non-required redteam job does); machines without Docker skip them.
func runner(t *testing.T) *Runner {
	t.Helper()
	if os.Getenv("EXEC_INTEGRATION") != "1" {
		t.Skip("EXEC_INTEGRATION not set")
	}
	return &Runner{WorkDir: t.TempDir(), Docker: "docker"}
}

func python(code, stdin string, limits Limits) Job {
	return Job{Language: "python", Files: map[string]string{"main.py": code}, Stdin: stdin, Limits: limits}
}

var small = Limits{MemoryMB: 64, CPUs: 1, Pids: 32, TmpMB: 8, Seconds: 3, OutputKB: 64}

func leftovers(t *testing.T, r *Runner) {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=velox.run=1").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("containers left: %s", out)
	}
	entries, err := os.ReadDir(r.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("run dirs left: %d", len(entries))
	}
}

func TestVerdicts(t *testing.T) {
	r := runner(t)
	cases := []struct {
		name, code, stdin string
		verdict           Verdict
		stdout            string
		exit              int
	}{
		{"hello", `print("hello")`, "", OK, "hello\n", 0},
		{"stdin", `import sys; print(sys.stdin.read().upper(), end="")`, "abc", OK, "ABC", 0},
		{"exit", `raise SystemExit(3)`, "", RuntimeError, "", 3},
		{"loop", `while True: pass`, "", TimeLimit, "", -1},
		{"memory", `x = b"x" * (1024 * 1024 * 1024)`, "", MemoryLimit, "", -1},
		{"flood", `while True: print("x" * 1000)`, "", OutputLimit, "", -1},
		{"stderr flood", `import sys
while True: sys.stderr.write("x" * 1000)`, "", OutputLimit, "", -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start := time.Now()
			got, err := r.Run(context.Background(), python(c.code, c.stdin, small))
			if err != nil {
				t.Fatal(err)
			}
			if got.Verdict != c.verdict {
				t.Fatalf("verdict %s, want %s (stderr %q)", got.Verdict, c.verdict, got.Stderr)
			}
			if c.stdout != "" && got.Stdout != c.stdout {
				t.Errorf("stdout %q", got.Stdout)
			}
			if c.exit >= 0 && got.ExitCode != c.exit {
				t.Errorf("exit %d", got.ExitCode)
			}
			// The limit, plus starting and removing a container.
			if time.Since(start) > time.Duration(small.Seconds)*time.Second+8*time.Second {
				t.Errorf("took %s", time.Since(start))
			}
			if len(got.Stdout) > small.OutputKB*1024 {
				t.Errorf("stdout not capped: %d bytes", len(got.Stdout))
			}
			leftovers(t, r)
		})
	}
}

func TestNoHostEnvironmentReachesTheRun(t *testing.T) {
	r := runner(t)
	t.Setenv("INTERVAL_SECRET", "s3cret")
	got, err := r.Run(context.Background(), python(
		`import os; print(sorted(os.environ))`, "", small))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Stdout, "INTERVAL_SECRET") || strings.Contains(got.Stdout, "s3cret") {
		t.Errorf("host env leaked: %s", got.Stdout)
	}
	leftovers(t, r)
}

func TestACancelledRunStopsPromptlyAndLeavesNothing(t *testing.T) {
	r := runner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err := r.Run(ctx, python(`while True: pass`, "", small))
	if err == nil {
		t.Fatal("no error")
	}
	if time.Since(start) > 15*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
	leftovers(t, r)
}

// P5-W28-S03 (EXEC-2, C-04): every launch language compiles once and runs; a compile
// error is its own verdict, with the compiler's words in stderr.
func TestEveryLanguage(t *testing.T) {
	r := runner(t)
	type program struct{ hello, echo, broken, loop string }
	programs := map[string]program{
		"python": {`print("hello")`, `import sys; print(sys.stdin.read(), end="")`, "", `while True: pass`},
		"javascript": {`console.log("hello")`,
			`process.stdout.write(require("fs").readFileSync(0, "utf8"))`, "", `while (true) {}`},
		"typescript": {`const s: string = "hello"; console.log(s)`,
			`import { readFileSync } from "node:fs"; const s: string = readFileSync(0, "utf8"); process.stdout.write(s)`,
			"", `while (true) {}`},
		"java": {
			`public class Main { public static void main(String[] a) { System.out.println("hello"); } }`,
			`public class Main { public static void main(String[] a) throws Exception { System.out.print(new String(System.in.readAllBytes())); } }`,
			`public class Main { public static void main(String[] a) { int x = "no"; } }`,
			`public class Main { public static void main(String[] a) { while (true) {} } }`},
		"cpp": {
			`#include <iostream>
int main() { std::cout << "hello\n"; }`,
			`#include <iostream>
int main() { std::cout << std::cin.rdbuf(); }`,
			`int main() { return nope; }`,
			`int main() { volatile int x = 0; while (true) { x++; } }`},
		"go": {
			`package main
import "fmt"
func main() { fmt.Println("hello") }`,
			`package main
import ("io"; "os")
func main() { io.Copy(os.Stdout, os.Stdin) }`,
			`package main
func main() { var x int = "no" }`,
			`package main
func main() { for {} }`},
	}
	for _, name := range Names() {
		p := programs[name]
		lang, _ := Lookup(name)
		job := func(code, stdin string) Job {
			return Job{Language: name, Files: map[string]string{lang.Main: code}, Stdin: stdin, Limits: small}
		}
		t.Run(name, func(t *testing.T) {
			got, err := r.Run(context.Background(), job(p.hello, ""))
			if err != nil || got.Verdict != OK || got.Stdout != "hello\n" {
				t.Fatalf("hello: %v %+v", err, got)
			}
			got, err = r.Run(context.Background(), job(p.echo, "echo me"))
			if err != nil || got.Verdict != OK || got.Stdout != "echo me" {
				t.Fatalf("echo: %v %+v", err, got)
			}
			if p.broken != "" {
				got, err = r.Run(context.Background(), job(p.broken, ""))
				if err != nil || got.Verdict != CompileError || got.Stderr == "" {
					t.Fatalf("compile error: %v %+v", err, got)
				}
			}
			got, err = r.Run(context.Background(), job(p.loop, ""))
			if err != nil || got.Verdict != TimeLimit {
				t.Fatalf("loop: %v %+v", err, got)
			}
			leftovers(t, r)
		})
	}
}

// A runtime with its own heap (the JVM, Node) stops at the job's memory, and that is a
// memory limit, not the program's failure.
func TestARuntimesOwnHeapLimitIsAMemoryLimit(t *testing.T) {
	r := runner(t)
	got, err := r.Run(context.Background(), Job{Language: "java", Limits: small, Files: map[string]string{"Main.java": `
import java.util.*;
public class Main { public static void main(String[] a) { List<byte[]> h = new ArrayList<>(); while (true) h.add(new byte[1 << 20]); } }`}})
	if err != nil || got.Verdict != MemoryLimit {
		t.Fatalf("%v %+v", err, got.Verdict)
	}
	leftovers(t, r)
}
