# The execution engine's red-team suite (EXEC-1)

EXEC-1 says the engine is chosen "by red-team test, not opinion": the same attacks run against Velox's engine (`backend/engine`, ported from Interval's `services/exec`; Interval's ADR-0041) and against Judge0, and an engine with any unsafe result loses. The suite reruns before every release.

## What it runs

`backend/redteam` has 22 attacks:

- **13 in Python:**
  - network;
  - a fork bomb;
  - the environment;
  - other processes;
  - writes outside `/tmp`;
  - another submission's files (concurrent);
  - memory;
  - CPU;
  - the wall clock;
  - CPU share;
  - an output flood;
  - disk;
  - privilege (capabilities, no_new_privs, seccomp, user namespaces, mount, chroot, setuid, the Docker socket).
- **A native probe and a memory hog** for each of JavaScript, TypeScript, C++ and Go.
- **Java's probe** only: its heap is the job's.

An attack is unsafe if:

- it prints `ESCAPED`;
- the host's canary appears;
- its verdict isn't one it allows;
- a probe never reaches `DONE`;
- or the engine errors.

A negative control runs the probes in a deliberately weak container and requires them to fail there, so a pass means something.

## Runs

| Where                                           | Command                                       | Result                                                            |
| ----------------------------------------------- | --------------------------------------------- | ----------------------------------------------------------------- |
| This repo, locally (Docker Desktop, runc)       | see below                                     | 2026-11-12: all 22 contained (OrbStack, after the port)           |
| CI (GitHub's runner Docker, runc)               | the `judge-engine` job                        | on every push to `main` and every pull request                    |
| An execution host with gVisor (EXEC-5)          | the same, with `EXEC_RUNTIME=runsc`           | **owner to run**                                                  |
| Judge0 CE                                       | below                                         | **owner to run**                                                  |

## Locally

```sh
docker build -q -t velox-exec-go:1 build/images/go
cd backend && EXEC_INTEGRATION=1 go test -p 1 -count=1 -v ./redteam/
```

## Judge0, on a Linux host (the owner's)

Judge0's isolate sandbox needs a privileged container and cgroup v1, which Docker Desktop on a Mac can't give. Only the 13 Python attacks run against it. Judge0 CE's other toolchains (Node 12, tsc 3.7, Go 1.13) predate what the native probes use, and those probes check our own images, not a sandbox.

1. Take a throwaway Linux VM with Docker. On a cgroup v2 distribution, boot with `systemd.unified_cgroup_hierarchy=0` (Judge0's own instructions).
2. Download Judge0 CE's release archive (the `judge0-v1.13.x.zip` from its GitHub releases) and unpack it.
3. Set the passwords in `judge0.conf`. Check its caps hold the suite's requests:
   - `MAX_MAX_FILE_SIZE` at least 4096 (KB);
   - `MAX_MEMORY_LIMIT` at least 131072 (KB);
   - `MAX_MAX_PROCESSES_AND_OR_THREADS` at least 64;
   - `MAX_CPU_TIME_LIMIT` and `MAX_WALL_TIME_LIMIT` at least 5.
4. In the unpacked Judge0 directory, run `docker compose up -d db redis`, then `docker compose up -d`.
5. From this repo, on a machine that can reach it:

   ```sh
   cd backend && JUDGE0_URL=http://<vm>:2358 go test -count=1 -v -run Judge0 ./redteam/
   ```

6. Sort every UNSAFE line, before the decision rule is applied, into one of:
   - **a real escape:** another run's data, the network, the host's environment, root, or a resource the run shouldn't have;
   - **a missing EXEC-3 control:** for example no seccomp filter (isolate doesn't use one), or no separate cap on `/dev/shm`. These count against the engine: EXEC-3 requires the control, not just the absence of an escape;
   - **a layout mismatch:** an environment variable isolate sets for itself, a path that exists only in our images. These don't count; note each one.
7. Fill in the Judge0 row above, then tear the VM down.

## The decision

- Any unsafe result loses.
- If both engines pass, Velox stays the engine (the owner's decision, Interval's ADR-0041), with EXEC-3 built in.
- No player code runs on Velox before the gVisor row is in.
