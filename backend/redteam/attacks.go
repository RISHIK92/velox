package redteam

import (
	"fmt"

	"github.com/rishik92/velox/engine"
)

// EXEC-1's attacks, in Python (the other languages' come in W28-S03). Each prints
// "ESCAPED: …" the moment it gets through, flushed, so a later kill can't lose it. A
// probe ends by printing DONE: one that crashed or hung before its checks proves
// nothing, so it isn't counted as contained. Thresholds come from the suite's limits.

const (
	ok      = "ok"
	timeout = "time_limit"
	memory  = "memory_limit"
	output  = "output_limit"
	// The neighbour's secret, and the line it prints once the secret is in place.
	neighbourToken = "REDTEAM-NEIGHBOUR-7f3a9c"
	neighbourReady = "NEIGHBOUR-READY"
)

// Line-buffered output, so an escape is printed before any kill.
const prelude = "import sys\nsys.stdout.reconfigure(line_buffering=True)\n"

func python(a Attack) Attack {
	a.Language = "python"
	a.Source = prelude + a.Source
	if !a.Resource {
		a.Source += "\nprint(\"DONE\")\n"
	}
	return a
}

// The names a run may see in its environment: the image's own ENV, Docker's defaults,
// PWD and SHLVL from the engine's wrapper shell, and LC_CTYPE, which Python sets itself
// when it coerces the C locale (PEP 538). Never the host's.
var pythonEnv = `{"PATH", "HOSTNAME", "HOME", "PWD", "SHLVL", "LANG", "LC_CTYPE", "GPG_KEY", "PYTHON_VERSION", "PYTHON_SHA256"}`

func Python(l engine.Limits) []Attack {
	return []Attack{
		python(Attack{Name: "network", Allowed: []string{ok}, CatchesWeak: true, Source: `
import os, socket
for nic in os.listdir("/sys/class/net"):
    if nic != "lo":
        print("ESCAPED: a network interface", nic)
routes = open("/proc/net/route").read().splitlines()[1:]
if routes:
    print("ESCAPED: a route", routes[0])
try:
    socket.create_connection(("1.1.1.1", 53), timeout=2)
    print("ESCAPED: tcp connect to 1.1.1.1:53")
except OSError as e:
    print("tcp blocked:", e)
try:
    print("ESCAPED: resolved", socket.gethostbyname("example.com"))
except OSError as e:
    print("dns blocked:", e)
`}),
		python(Attack{Name: "fork bomb", Allowed: []string{ok}, Source: fmt.Sprintf(`
import os, time
n = 0
try:
    while True:
        if os.fork() == 0:
            time.sleep(5)
            os._exit(0)
        n += 1
        if n >= %d:
            print("ESCAPED: forked", n, "processes past the limit of %d")
            break
except OSError as e:
    print("refused after", n, e)
`, l.Pids*2, l.Pids)}),
		python(Attack{Name: "environment", Allowed: []string{ok}, Source: `
import os
allowed = ` + pythonEnv + `
for name in os.environ:
    if name not in allowed:
        print("ESCAPED: environment variable", name)
print(sorted(os.environ))
`}),
		python(Attack{Name: "other processes", Allowed: []string{ok}, Source: `
import os
# Every visible process must be this run's own: pid 1 (an engine's init or the program
# itself), the program's ancestors, or itself. Anything else is another's.
def parent(pid):
    for line in open(f"/proc/{pid}/status"):
        if line.startswith("PPid:"):
            return int(line.split()[1])
    return 0
own = {1, os.getpid()}
p = os.getpid()
while p > 1:
    p = parent(p)
    own.add(p)
pids = {int(p) for p in os.listdir("/proc") if p.isdigit()}
first = open("/proc/1/cmdline", "rb").read().replace(b"\0", b" ").decode()
print("pids", sorted(pids), "own", sorted(own), "pid 1:", first)
for other in pids - own:
    try:
        cmd = open(f"/proc/{other}/cmdline", "rb").read().replace(b"\0", b" ").decode()
    except OSError:
        continue
    print("ESCAPED: sees another process", other, cmd)
print(open("/proc/1/environ", "rb").read())
`}),
		python(Attack{Name: "writes outside /tmp", Allowed: []string{ok}, CatchesWeak: true, Source: `
import errno
# /var/tmp is writable by anyone in the image: only a read-only filesystem stops it. A
# path that isn't there (another engine's layout) can't be written, which is fine.
for path in ["/code/x", "/x", "/etc/x", "/usr/x", "/var/tmp/x"]:
    try:
        with open(path, "w") as f:
            f.write("x")
        print("ESCAPED: wrote", path)
    except OSError as e:
        if e.errno == errno.ENOENT:
            print("absent", path)
        elif e.errno != errno.EROFS:
            print("ESCAPED: only permissions stopped", path, e)
        else:
            print("read-only", path)
`}),
		python(Attack{Name: "another submission's files", Allowed: []string{ok}, Stdin: neighbourToken, Source: `
import os, time
token = sys.stdin.read().strip().encode()
# Its own copy proves the search can find one: a search that finds nothing proves nothing.
open("/tmp/own-copy", "wb").write(token)
found = set()

def look(path):
    try:
        if os.path.getsize(path) < 1 << 20 and token in open(path, "rb").read():
            found.add(path)
    except OSError:
        pass

# Once over the whole filesystem (not /proc, /sys, /usr or /dev, but /dev/shm)...
for root, dirs, files in os.walk("/"):
    if root in ("/proc", "/sys", "/usr", "/dev"):
        dirs[:] = ["shm"] if root == "/dev" else []
        continue
    for name in files:
        look(os.path.join(root, name))
# ...then again and again where a neighbour would leave something, for 3 seconds.
deadline = time.time() + 3
while time.time() < deadline:
    for base in ["/tmp", "/dev/shm", "/code"]:
        try:
            names = os.listdir(base)
        except OSError:
            continue
        for name in names:
            look(os.path.join(base, name))
    for pid in os.listdir("/proc"):
        if pid.isdigit():
            for where in ["/tmp/secret", "/dev/shm/secret"]:
                look(f"/proc/{pid}/root{where}")
    time.sleep(0.2)
if "/tmp/own-copy" not in found:
    print("ESCAPED: the search missed its own copy, so it proves nothing")
found.discard("/tmp/own-copy")
if found:
    print("ESCAPED: another run's token in", sorted(found))
else:
    print("nothing of another run's")
`,
			// The neighbour: its token in its own source (/code), /tmp and /dev/shm.
			Beside: &Attack{Name: "neighbour", Language: "python", Allowed: []string{ok}, Source: prelude + `
import time
open("/tmp/secret", "w").write("` + neighbourToken + `")
open("/dev/shm/secret", "w").write("` + neighbourToken + `")
print("` + neighbourReady + `")
time.sleep(4)
`}}),
		python(Attack{Name: "memory", Resource: true, Allowed: []string{memory}, Source: fmt.Sprintf(`
chunks = []
for i in range(%d):
    chunks.append(b"x" * (64 << 20))
print("ESCAPED: held", len(chunks) * 64, "MB under a %d MB limit")
`, l.MemoryMB*4/64, l.MemoryMB)}),
		python(Attack{Name: "cpu", Resource: true, Allowed: []string{timeout}, Source: `
while True:
    pass
`}),
		python(Attack{Name: "wall clock", Resource: true, Allowed: []string{timeout}, Source: `
import time
time.sleep(1000)
print("ESCAPED: slept past the limit")
`}),
		python(Attack{Name: "cpu share", Allowed: []string{ok}, Source: fmt.Sprintf(`
import os, resource, time
start = time.time()
kids = []
for _ in range(4):
    pid = os.fork()
    if pid == 0:
        end = time.time() + 2
        while time.time() < end:
            pass
        os._exit(0)
    kids.append(pid)
for pid in kids:
    os.waitpid(pid, 0)
wall = time.time() - start
used = resource.getrusage(resource.RUSAGE_CHILDREN)
cpu = used.ru_utime + used.ru_stime
print("cpu", round(cpu, 2), "s in", round(wall, 2), "s wall")
if cpu > wall * %v * 1.5:
    print("ESCAPED: more than %v CPUs")
`, l.CPUs, l.CPUs)}),
		python(Attack{Name: "output flood", Resource: true, Allowed: []string{output}, Source: `
while True:
    print("x" * 4096)
`}),
		python(Attack{Name: "disk: /tmp and /dev/shm", Allowed: []string{ok}, CatchesWeak: true, Source: fmt.Sprintf(`
import os
cap = %d
for base in ["/tmp", "/dev/shm"]:
    total = 0
    try:
        # Files of 4 MB, under the per-file limit: only the mount's size stops these.
        for i in range(cap // 4 + 3):
            with open(f"{base}/fill{i}", "wb") as f:
                for _ in range(4):
                    f.write(b"x" * (1 << 20))
                    total += 1
            if total > cap:
                print("ESCAPED: wrote", total, "MB to", base, "past", cap)
                break
    except OSError as e:
        print(base, "stopped at", total, "MB:", e.errno)
    for name in os.listdir(base):
        if name.startswith("fill"):
            os.remove(f"{base}/{name}")
`, l.TmpMB)}),
		python(Attack{Name: "privilege", Allowed: []string{ok}, CatchesWeak: true, Source: `
import ctypes, os
status = dict(line.split(":\t", 1) for line in open("/proc/self/status").read().splitlines() if ":\t" in line)
for cap in ["CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"]:
    if int(status[cap].strip(), 16) != 0:
        print("ESCAPED: capabilities", cap, status[cap].strip())
if status["NoNewPrivs"].strip() != "1":
    print("ESCAPED: no_new_privs is off")
if status["Seccomp"].strip() != "2":
    print("ESCAPED: no seccomp filter")
libc = ctypes.CDLL(None, use_errno=True)
CLONE_NEWUSER = 0x10000000
if libc.unshare(CLONE_NEWUSER) == 0:
    print("ESCAPED: unshare(CLONE_NEWUSER)")
else:
    print("unshare refused", ctypes.get_errno())
if libc.mount(b"none", b"/tmp", b"tmpfs", 0, None) == 0:
    print("ESCAPED: mount")
else:
    print("mount refused", ctypes.get_errno())
for name, call in [("chroot", lambda: os.chroot("/tmp")), ("setuid(0)", lambda: os.setuid(0))]:
    try:
        call()
        print("ESCAPED:", name)
    except OSError as e:
        print("refused", name, e.errno)
for path in ["/etc/shadow", "/var/run/docker.sock", "/run/docker.sock"]:
    if os.path.exists(path):
        try:
            open(path, "rb").read(1)
            print("ESCAPED: read", path)
        except OSError as e:
            print("refused", path, e.errno)
if os.geteuid() == 0:
    print("ESCAPED: running as root")
`}),
	}
}
