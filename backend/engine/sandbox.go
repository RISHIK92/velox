package engine

import (
	"errors"
	"fmt"
	"strconv"
)

// P5-W28-S01 (EXEC-3; Q-P5-02, Interval's ADR-0041): what isolates one run. A fresh
// container for every run, with no network, a read-only filesystem except a capped
// /tmp, a non-root user with no capabilities, limits on memory, CPU and processes, and
// no environment from the host: only the image's own ENV and Docker's defaults. The run's
// files are mounted read-only from a directory of its own, so it can't reach another
// run's. On the execution hosts the runtime is gVisor (EXEC_RUNTIME=runsc).

// Limits for one run. Seconds is wall-clock; OutputKB caps stdout and stderr each.
type Limits struct {
	MemoryMB int
	CPUs     float64
	Pids     int
	TmpMB    int
	Seconds  int
	OutputKB int
}

var errLimits = errors.New("limits out of range")

// Docker reads a zero memory, pids or CPU limit as none at all, so every limit must be
// set and within reason before a container is made: a missing one never fails open.
func (l Limits) validate() error {
	switch {
	case l.MemoryMB < 16 || l.MemoryMB > 4096,
		l.CPUs < 0.1 || l.CPUs > 4,
		l.Pids < 8 || l.Pids > 1024,
		l.TmpMB < 1 || l.TmpMB > 1024,
		l.Seconds < 1 || l.Seconds > 60,
		l.OutputKB < 1 || l.OutputKB > 16384:
		return fmt.Errorf("%w: %+v", errLimits, l)
	}
	return nil
}

// Spec is one container: its name, image and command, and the run directory mounted
// read-only at /code.
type Spec struct {
	Name   string
	Image  string
	Cmd    []string
	RunDir string
	Limits Limits
}

// nobody:nogroup, present in every base image, so the files need no chown.
const sandboxUser = "65534:65534"

func (s Spec) createArgs(runtime string) []string {
	memory := fmt.Sprintf("%dm", s.Limits.MemoryMB)
	args := []string{
		"create",
		"--name", s.Name,
		"-i",
		"--network", "none",
		"--read-only",
		"--tmpfs", fmt.Sprintf("/tmp:rw,exec,nosuid,nodev,size=%dm", s.Limits.TmpMB),
		// /dev/shm is writable despite --read-only: kept as small as /tmp.
		"--shm-size", fmt.Sprintf("%dm", s.Limits.TmpMB),
		"--user", sandboxUser,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", strconv.Itoa(s.Limits.Pids),
		"--ulimit", "nofile=256:256",
		"--ulimit", fmt.Sprintf("fsize=%d:%d", s.Limits.TmpMB<<20, s.Limits.TmpMB<<20),
		"--memory", memory,
		"--memory-swap", memory,
		"--cpus", strconv.FormatFloat(s.Limits.CPUs, 'f', -1, 64),
		"--env-file", "/dev/null",
		// Output reaches us only through the attach: none is kept on the host's disk.
		"--log-driver", "none",
		"--workdir", "/code",
		"-v", s.RunDir + ":/code:ro",
		"--label", "velox.run=1",
		// An image's own entrypoint (Temurin's and Node's have one) never runs: only
		// the command given here does.
		"--entrypoint", "",
	}
	if runtime != "" {
		args = append(args, "--runtime", runtime)
	}
	args = append(args, s.Image)
	return append(args, s.Cmd...)
}
