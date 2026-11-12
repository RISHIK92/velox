package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rishik92/velox/checker"
)

// P5-W30-S01: exec check passes, fails, or refuses the problem's own bad data.
func TestCheckCommand(t *testing.T) {
	dir := t.TempDir()
	file := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	scenario := file("s.json", `{"calls":[{"method":"new","args":[]},{"method":"get","args":[]}]}`)
	expected := file("e.json", `[{"call":0,"value":null},{"call":1,"value":7}]`)
	args := func(stdout string) []string {
		return []string{"--scenario", scenario, "--expected", expected, "--stdout", file("o.txt", stdout)}
	}
	if passed, err := check(args("{\"call\":0,\"value\":null}\n{\"call\":1,\"value\":7}\n")); err != nil || !passed {
		t.Fatalf("honest: %v %v", passed, err)
	}
	if passed, err := check(args("{\"call\":0,\"value\":null}\n{\"call\":1,\"value\":8}\n")); err != nil || passed {
		t.Fatalf("wrong: %v %v", passed, err)
	}
	bad := []string{"--scenario", scenario, "--expected", file("bad.json", `[{"call":0,"value":null}]`), "--stdout", file("o2.txt", "")}
	if _, err := check(bad); !errors.Is(err, checker.ErrProblem) {
		t.Fatalf("problem: %v", err)
	}
}

func TestCheckRefusesBadInputAsTheProblemsFault(t *testing.T) {
	dir := t.TempDir()
	file := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	scenario := file("s.json", `{"calls":[{"method":"new","args":[]}]}`)
	expected := file("e.json", `[{"call":0,"value":null}]`)
	stdout := file("o.txt", "{\"call\":0,\"value\":null}\n")
	for name, args := range map[string][]string{
		"malformed expected": {"--scenario", scenario, "--expected", file("bad.json", `[{`), "--stdout", stdout},
		"a bad check":        {"--scenario", scenario, "--expected", expected, "--stdout", stdout, "--checks", file("c.json", `[{"method":"get","compare":"exact","tolerance":0.01}]`)},
	} {
		if _, err := check(args); !errors.Is(err, checker.ErrProblem) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := check([]string{"--scenario", scenario}); err == nil {
		t.Error("missing flags accepted")
	}
	if _, err := check([]string{"--scenario", scenario, "--expected", expected, "--stdout", stdout, "extra"}); err == nil {
		t.Error("an extra argument accepted")
	}
}

func TestRunRefusesLimitsOutOfRange(t *testing.T) {
	source := filepath.Join(t.TempDir(), "main.py")
	if err := os.WriteFile(source, []byte("print(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Refused before Docker is reached.
	if err := run([]string{"--language", "python", "--file", source, "--seconds", "0"}); err == nil {
		t.Error("zero seconds accepted")
	}
}
