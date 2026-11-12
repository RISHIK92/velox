package redteam

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// P5-W28-S04 (EXEC-1): Judge0, the engine ours is compared against, through its REST
// API with the suite's limits. Its run is the owner's, on a Linux host (Judge0's
// isolate needs a privileged container and cgroup v1): docs/runbooks/exec-redteam.md.
type Judge0Engine struct {
	URL    string
	Client *http.Client
}

// Judge0 CE 1.13's language ids for the launch languages.
var judge0Languages = map[string]int{
	"python": 71, "javascript": 63, "typescript": 74, "java": 62, "cpp": 54, "go": 60,
}

var errJudge0 = errors.New("judge0")

type judge0Request struct {
	Source       string  `json:"source_code"`
	Language     int     `json:"language_id"`
	Stdin        string  `json:"stdin"`
	CPUTime      float64 `json:"cpu_time_limit"`
	WallTime     float64 `json:"wall_time_limit"`
	MemoryKB     int     `json:"memory_limit"`
	Processes    int     `json:"max_processes_and_or_threads"`
	MaxFileKB    int     `json:"max_file_size"`
	Network      bool    `json:"enable_network"`
	NumberOfRuns int     `json:"number_of_runs"`
}

type judge0Status struct {
	ID          int    `json:"id"`
	Description string `json:"description"`
}

type judge0Response struct {
	Stdout     *string      `json:"stdout"`
	Status     judge0Status `json:"status"`
	MemoryKB   *int         `json:"memory"`
	ExitSignal *int         `json:"exit_signal"`
	// Base64, as every text field is with base64_encoded=true.
	Message string `json:"message"`
}

func (j Judge0Engine) Run(ctx context.Context, language, source, stdin string) (Outcome, error) {
	id, ok := judge0Languages[language]
	if !ok {
		return Outcome{}, fmt.Errorf("%w: no language id for %q", errJudge0, language)
	}
	b64 := base64.StdEncoding.EncodeToString
	body, err := json.Marshal(judge0Request{
		Source:    b64([]byte(source)),
		Language:  id,
		Stdin:     b64([]byte(stdin)),
		CPUTime:   float64(Limits.Seconds),
		WallTime:  float64(Limits.Seconds),
		MemoryKB:  Limits.MemoryMB * 1024,
		Processes: Limits.Pids,
		// Judge0 CE's stock judge0.conf caps this at 4096 KB (MAX_MAX_FILE_SIZE).
		MaxFileKB:    min(Limits.TmpMB*1024, 4096),
		Network:      false,
		NumberOfRuns: 1,
	})
	if err != nil {
		return Outcome{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(j.URL, "/")+"/submissions?base64_encoded=true&wait=true", bytes.NewReader(body))
	if err != nil {
		return Outcome{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := j.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return Outcome{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return Outcome{}, fmt.Errorf("%w: %s: %s", errJudge0, resp.Status, detail)
	}
	var got judge0Response
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		return Outcome{}, err
	}
	verdict, err := judge0Verdict(got, Limits.MemoryMB*1024)
	if err != nil {
		return Outcome{}, err
	}
	out := ""
	if got.Stdout != nil {
		raw, err := base64.StdEncoding.DecodeString(*got.Stdout)
		if err != nil {
			return Outcome{}, fmt.Errorf("%w: stdout: %w", errJudge0, err)
		}
		out = string(raw)
	}
	return Outcome{Verdict: verdict, Stdout: out}, nil
}

// Judge0's statuses in the suite's verdicts. It has no memory status: a run killed by
// SIGKILL at its memory limit is one. Status 8 (SIGXFSZ) is its output limit: stdout is
// a file capped by max_file_size. Its own errors are engine errors, never a pass.
func judge0Verdict(r judge0Response, limitKB int) (string, error) {
	switch id := r.Status.ID; {
	case id == 3:
		return ok, nil
	case id == 5:
		return timeout, nil
	case id == 6:
		return "compile_error", nil
	case id == 8:
		return output, nil
	case id >= 7 && id <= 12:
		if r.ExitSignal != nil && *r.ExitSignal == 9 && r.MemoryKB != nil && *r.MemoryKB*10 >= limitKB*9 {
			return memory, nil
		}
		return "runtime_error", nil
	default:
		message, _ := base64.StdEncoding.DecodeString(r.Message)
		return "", fmt.Errorf("%w: status %d %s: %s", errJudge0, id, r.Status.Description, message)
	}
}
