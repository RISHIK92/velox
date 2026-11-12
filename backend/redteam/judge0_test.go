package redteam

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestJudge0IsAskedWithTheSuitesLimitsAndAnswersAreMapped(t *testing.T) {
	var asked judge0Request
	answer := `{"stdout":"` + base64.StdEncoding.EncodeToString([]byte("hi\n")) + `","status":{"id":3}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/submissions" || r.URL.Query().Get("wait") != "true" || r.URL.Query().Get("base64_encoded") != "true" {
			t.Errorf("asked %s", r.URL)
		}
		if err := json.NewDecoder(r.Body).Decode(&asked); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()
	o, err := Judge0Engine{URL: srv.URL}.Run(context.Background(), "python", "print('hi')", "in")
	if err != nil || o != (Outcome{Verdict: ok, Stdout: "hi\n"}) {
		t.Fatalf("%v %+v", err, o)
	}
	src, _ := base64.StdEncoding.DecodeString(asked.Source)
	want := judge0Request{
		Source: asked.Source, Language: 71, Stdin: base64.StdEncoding.EncodeToString([]byte("in")),
		CPUTime: float64(Limits.Seconds), WallTime: float64(Limits.Seconds), MemoryKB: Limits.MemoryMB * 1024,
		Processes: Limits.Pids, MaxFileKB: 4096, Network: false, NumberOfRuns: 1,
	}
	if string(src) != "print('hi')" || asked != want {
		t.Errorf("request %+v, want %+v", asked, want)
	}
}

func TestJudge0FailuresAreEngineErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"max_file_size":["must be less than or equal to 4096"]}`, http.StatusUnprocessableEntity)
	}))
	defer srv.Close()
	if _, err := (Judge0Engine{URL: srv.URL}).Run(context.Background(), "python", "x", ""); !errors.Is(err, errJudge0) {
		t.Errorf("422: %v", err)
	}
	if _, err := (Judge0Engine{URL: srv.URL}).Run(context.Background(), "cobol", "x", ""); !errors.Is(err, errJudge0) {
		t.Errorf("unknown language: %v", err)
	}
}

func TestJudge0StatusesBecomeTheSuitesVerdicts(t *testing.T) {
	nine, big, small := 9, 131072, 1024
	cases := []struct {
		r    judge0Response
		want string
	}{
		{judge0Response{Status: judge0Status{ID: 3}}, ok},
		{judge0Response{Status: judge0Status{ID: 5}}, timeout},
		{judge0Response{Status: judge0Status{ID: 6}}, "compile_error"},
		{judge0Response{Status: judge0Status{ID: 8}}, output},
		{judge0Response{Status: judge0Status{ID: 12}, ExitSignal: &nine, MemoryKB: &big}, memory},
		{judge0Response{Status: judge0Status{ID: 12}, ExitSignal: &nine, MemoryKB: &small}, "runtime_error"},
		{judge0Response{Status: judge0Status{ID: 11}}, "runtime_error"},
	}
	for _, c := range cases {
		if got, err := judge0Verdict(c.r, 131072); err != nil || got != c.want {
			t.Errorf("%+v: %s %v", c.r.Status, got, err)
		}
	}
	// Its own failures are never a verdict.
	if _, err := judge0Verdict(judge0Response{Status: judge0Status{ID: 13}}, 131072); !errors.Is(err, errJudge0) {
		t.Error(err)
	}
}

// EXEC-1's comparison run, the owner's: JUDGE0_URL=http://host:2358 (the runbook). The
// Python suite only: Judge0 CE's toolchains (Node 12, tsc 3.7, Go 1.13) predate what
// the native probes use, and those probes check our own images, not a sandbox.
func TestJudge0ContainsEveryAttack(t *testing.T) {
	url := os.Getenv("JUDGE0_URL")
	if url == "" {
		t.Skip("JUDGE0_URL not set")
	}
	e := Judge0Engine{URL: url, Client: &http.Client{Timeout: 2 * time.Minute}}
	for _, a := range Python(Limits) {
		t.Run(a.Name, func(t *testing.T) {
			runAttack(t, e, a, "")
		})
	}
}
