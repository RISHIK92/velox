package engine

import "testing"

// Interval's Q-WD-17: a run that didn't finish, said for its player from the
// toolchain's own output: the error, and the line of their file it names. The stderr
// here is each toolchain's real output, with the sandbox's /code paths.
func TestDiagnoseFindsTheErrorAndItsLineInThePlayersFile(t *testing.T) {
	cases := []struct {
		name, language string
		verdict        Verdict
		file, stderr   string
		message        string
		line           int
	}{
		{"python syntax", "python", RuntimeError, "solution.py", `Traceback (most recent call last):
  File "/code/main.py", line 12, in <module>
    from solution import A  # noqa: E402
  File "/code/solution.py", line 3
    return (
           ^
SyntaxError: '(' was never closed
`, "runtime error: SyntaxError: '(' was never closed", 3},
		{"python, innermost frame", "python", RuntimeError, "solution.py", `Traceback (most recent call last):
  File "/code/main.py", line 12, in <module>
  File "/code/solution.py", line 9, in <module>
    A().f()
  File "/code/solution.py", line 4, in f
    return {}["x"]
KeyError: 'x'
`, "runtime error: KeyError: 'x'", 4},
		{"javascript syntax", "javascript", RuntimeError, "solution.js", `/code/solution.js:3
    return 1 +;
              ^

SyntaxError: Unexpected token ';'
    at wrapSafe (node:internal/modules/cjs/loader:1713:18)
    at Object.<anonymous> (/code/main.js:12:15)

Node.js v22.22.3
`, "runtime error: SyntaxError: Unexpected token ';'", 3},
		{"typescript strip-only", "typescript", RuntimeError, "solution.ts", `node:internal/modules/run_main:123
    triggerUncaughtException(
    ^

/code/solution.ts:1
enum E { X }
^^^^^^^^^^^^

SyntaxError [ERR_UNSUPPORTED_TYPESCRIPT_SYNTAX]: TypeScript enum is not supported in strip-only mode
    at parseTypeScript (node:internal/modules/typescript:63:40)
`, "runtime error: SyntaxError [ERR_UNSUPPORTED_TYPESCRIPT_SYNTAX]: TypeScript enum is not supported in strip-only mode", 1},
		{"java compile", "java", CompileError, "Bank.java", `/code/Bank.java:3: error: cannot find symbol
  int f() { return x; }
                   ^
  symbol:   variable x
1 error
`, "compile error: error: cannot find symbol", 3},
		{"java compile, in the harness", "java", CompileError, "Bank.java", `/code/Main.java:40: error: cannot find symbol
1 error
`, "compile error: /code/Main.java:40: error: cannot find symbol", 0},
		{"c++ compile", "cpp", CompileError, "solution.hpp", "In file included from /code/main.cpp:3:\n/code/solution.hpp: In member function ‘int A::f()’:\n/code/solution.hpp:3:20: error: ‘x’ was not declared in this scope\n    3 |   int f() { return x; }\n", "compile error: error: \u2018x\u2019 was not declared in this scope", 3},
		{"c++ crash", "cpp", RuntimeError, "solution.hpp", "", "runtime error: the program crashed", 0},
		{"go compile", "go", CompileError, "solution.go", "# command-line-arguments\n./solution.go:6:9: undefined: x\n", "compile error: undefined: x", 6},
		{"go crash", "go", RuntimeError, "solution.go", "runtime: goroutine stack exceeds 1000000000-byte limit\nfatal error: stack overflow\n\ngoroutine 1 gp=0xc000002380 m=0 mp=0x5a4f20 [running]:\nmain.(*A).F(0xc000012345, 0x1)\n\t/code/solution.go:6 +0x45 fp=0xc0200e0398\nmain.main()\n\t/code/main.go:90 +0x1d\n", "runtime error: fatal error: stack overflow", 6},
		{"no player file", "go", CompileError, "", "./solution.go:6:9: undefined: x\n", "compile error: ./solution.go:6:9: undefined: x", 0},
		{"time", "python", TimeLimit, "solution.py", `File "/code/solution.py", line 2`, "time limit: it ran too long", 0},
		{"memory", "java", MemoryLimit, "Bank.java", "", "memory limit: it ran out of memory", 0},
		{"output", "go", OutputLimit, "solution.go", "", "output limit: it printed too much", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			message, line := Diagnose(c.language, c.verdict, c.stderr, c.file)
			if message != c.message || line != c.line {
				t.Fatalf("got %q at %d, want %q at %d", message, line, c.message, c.line)
			}
		})
	}
}
