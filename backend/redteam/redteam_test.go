package redteam

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rishik92/velox/engine"
)

func integration(t *testing.T) {
	t.Helper()
	if os.Getenv("EXEC_INTEGRATION") != "1" {
		t.Skip("EXEC_INTEGRATION not set")
	}
}

// The suite against services/exec. EXEC_INTEGRATION=1 runs it (Docker); `pnpm redteam`
// sets it.
func TestExecEngineContainsEveryAttack(t *testing.T) {
	integration(t)
	canaryBytes := make([]byte, 12)
	_, _ = rand.Read(canaryBytes)
	canary := "canary-" + hex.EncodeToString(canaryBytes)
	// In the engine's own environment: it must never reach a run.
	t.Setenv("INTERVAL_REDTEAM_CANARY", canary)
	e := NewExecEngine()
	for _, a := range append(Python(Limits), Native(Limits.MemoryMB)...) {
		t.Run(a.Name, func(t *testing.T) {
			runAttack(t, e, a, canary)
		})
	}
}

// runAttack runs one attack (and its neighbour, at the same time) and fails on anything
// it wasn't contained by. An engine error is a failure, never a skip.
func runAttack(t *testing.T, e Engine, a Attack, canary string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	if a.Beside != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := e.Run(ctx, a.Beside.Language, a.Beside.Source, a.Beside.Stdin)
			if err != nil {
				t.Errorf("neighbour: %v", err)
				return
			}
			// Its secret must have been in place, or the search proves nothing.
			if !strings.Contains(n.Stdout, neighbourReady) || n.Verdict != ok {
				t.Errorf("neighbour never got ready: %s %q", n.Verdict, n.Stdout)
			}
		}()
		// Started first, so its secret is there while the search runs.
		time.Sleep(500 * time.Millisecond)
	}
	o, err := e.Run(ctx, a.Language, a.Source, a.Stdin)
	wg.Wait()
	if err != nil {
		t.Fatalf("engine error (counts as a failure): %v", err)
	}
	if why := Unsafe(a, o, canary); why != "" {
		t.Errorf("UNSAFE: %s\n%s", why, o.Stdout)
		return
	}
	t.Logf("contained (%s): %.400s", o.Verdict, o.Stdout)
}

// weakEngine is a sandbox with none of EXEC-3 but a non-root user: the attacks marked
// CatchesWeak must report it, or their passes on the real engine mean nothing.
type weakEngine struct{ dir string }

func (w weakEngine) Run(ctx context.Context, language, source, stdin string) (Outcome, error) {
	lang, _ := engine.Lookup(language)
	run, err := os.MkdirTemp(w.dir, "weak-")
	if err != nil {
		return Outcome{}, err
	}
	defer os.RemoveAll(run)
	if err := os.Chmod(run, 0o755); err != nil {
		return Outcome{}, err
	}
	if err := os.WriteFile(filepath.Join(run, lang.Main), []byte(source), 0o644); err != nil {
		return Outcome{}, err
	}
	args := append([]string{"run", "--rm", "-i", "--user", "65534:65534", "-v", run + ":/code:ro", lang.Image}, lang.Cmd(Limits.MemoryMB)...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		return Outcome{}, err
	}
	// A compiled language's READY line, as the engine strips it.
	return Outcome{Verdict: ok, Stdout: strings.TrimPrefix(string(out), "\x1eREADY\n")}, nil
}

func TestTheAttacksCatchAWeakSandbox(t *testing.T) {
	integration(t)
	w := weakEngine{dir: t.TempDir()}
	for _, a := range append(Python(Limits), Native(Limits.MemoryMB)...) {
		if !a.CatchesWeak {
			continue
		}
		t.Run(a.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			o, err := w.Run(ctx, a.Language, a.Source, a.Stdin)
			if err != nil {
				t.Fatal(err)
			}
			if Unsafe(a, o, "") == "" {
				t.Errorf("a weak sandbox passed: %s", o.Stdout)
			}
		})
	}
}

// The suite must catch a broken engine, or its passes mean nothing.
func TestTheSuiteFailsABrokenEngine(t *testing.T) {
	a := Attack{Name: "x", Allowed: []string{"ok"}}
	for name, o := range map[string]Outcome{
		"escape":     {Verdict: "ok", Stdout: "trying\nESCAPED: tcp connect\nDONE\n"},
		"canary":     {Verdict: "ok", Stdout: "{'X': 'canary-1'}\nDONE\n"},
		"bad limit":  {Verdict: "time_limit", Stdout: "DONE\n"},
		"never done": {Verdict: "ok", Stdout: "blocked\n"},
	} {
		if Unsafe(a, o, "canary-1") == "" {
			t.Errorf("%s: passed", name)
		}
	}
	if why := Unsafe(a, Outcome{Verdict: "ok", Stdout: "blocked\nDONE\n"}, "canary-1"); why != "" {
		t.Errorf("contained flagged: %s", why)
	}
	every := Python(Limits)
	for _, a := range every {
		if a.Resource && strings.Contains(a.Source, "DONE") {
			t.Errorf("%s: a resource attack can't end in DONE", a.Name)
		}
	}
}
