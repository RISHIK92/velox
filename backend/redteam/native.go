package redteam

import (
	"fmt"

	"github.com/rishik92/velox/engine"
)

// P5-W28-S03: the same sandbox seen from each other launch language, through a native
// probe of the checks that don't depend on the language (environment, capabilities,
// no_new_privs, seccomp, the read-only filesystem, network interfaces) and a memory
// hog. The isolation is the container's, so these confirm each image adds nothing: an
// environment variable, a capability, a writable path. Python keeps the full suite.

// Each image's own ENV names, Docker's HOSTNAME and HOME, and PWD and SHLVL from the
// engine's own wrapper shell: never the host's.
var imageEnv = map[string][]string{
	"javascript": {"PATH", "NODE_VERSION", "YARN_VERSION", "HOSTNAME", "HOME", "PWD", "SHLVL"},
	"typescript": {"PATH", "NODE_VERSION", "YARN_VERSION", "HOSTNAME", "HOME", "PWD", "SHLVL"},
	"java":       {"PATH", "JAVA_HOME", "LANG", "LANGUAGE", "LC_ALL", "JAVA_VERSION", "HOSTNAME", "HOME", "PWD", "SHLVL"},
	"cpp":        {"PATH", "GPG_KEYS", "GCC_MIRRORS", "GCC_VERSION", "HOSTNAME", "HOME", "PWD", "SHLVL"},
	"go":         {"PATH", "GOLANG_VERSION", "GOTOOLCHAIN", "GOPATH", "HOSTNAME", "HOME", "PWD", "SHLVL"},
}

var writePaths = []string{"/code/x", "/x", "/etc/x", "/usr/x", "/var/tmp/x"}

func quoted(names []string) string {
	s := ""
	for i, n := range names {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("%q", n)
	}
	return s
}

const jsProbe = `
const fs = require("fs");
const allowed = new Set([%s]);
for (const name of Object.keys(process.env)) if (!allowed.has(name)) console.log("ESCAPED: environment variable " + name);
const status = Object.fromEntries(fs.readFileSync("/proc/self/status", "utf8").split("\n").filter((l) => l.includes(":\t")).map((l) => l.split(":\t")));
for (const cap of ["CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"]) if (BigInt("0x" + status[cap].trim()) !== 0n) console.log("ESCAPED: capabilities " + cap);
if (status.NoNewPrivs.trim() !== "1") console.log("ESCAPED: no_new_privs is off");
if (status.Seccomp.trim() !== "2") console.log("ESCAPED: no seccomp filter");
for (const path of [%s]) {
  try { fs.writeFileSync(path, "x"); console.log("ESCAPED: wrote " + path); }
  catch (e) { if (e.code !== "EROFS") console.log("ESCAPED: only " + e.code + " stopped " + path); }
}
for (const nic of fs.readdirSync("/sys/class/net")) if (nic !== "lo") console.log("ESCAPED: a network interface " + nic);
console.log("DONE");
`

const jsHog = `
const held = [];
for (let i = 0; i < %d; i++) held.push(Buffer.alloc(64 << 20, 1));
console.log("ESCAPED: held " + held.length * 64 + " MB");
`

const javaProbe = `
import java.io.*;
import java.nio.file.*;
import java.util.*;

public class Main {
  public static void main(String[] args) throws Exception {
    Set<String> allowed = Set.of(%s);
    for (String name : System.getenv().keySet()) if (!allowed.contains(name)) System.out.println("ESCAPED: environment variable " + name);
    Map<String, String> status = new HashMap<>();
    for (String line : Files.readAllLines(Path.of("/proc/self/status"))) {
      int i = line.indexOf(":\t");
      if (i > 0) status.put(line.substring(0, i), line.substring(i + 2).trim());
    }
    for (String cap : List.of("CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"))
      if (Long.parseUnsignedLong(status.get(cap), 16) != 0) System.out.println("ESCAPED: capabilities " + cap);
    if (!status.get("NoNewPrivs").equals("1")) System.out.println("ESCAPED: no_new_privs is off");
    if (!status.get("Seccomp").equals("2")) System.out.println("ESCAPED: no seccomp filter");
    for (String path : List.of(%s)) {
      try (FileOutputStream f = new FileOutputStream(path)) {
        System.out.println("ESCAPED: wrote " + path);
      } catch (IOException e) {
        if (e.getMessage() == null || !e.getMessage().contains("Read-only file system")) System.out.println("ESCAPED: only " + e.getMessage() + " stopped " + path);
      }
    }
    for (String nic : new File("/sys/class/net").list()) if (!nic.equals("lo")) System.out.println("ESCAPED: a network interface " + nic);
    System.out.println("DONE");
  }
}
`

const cppProbe = `
#include <cerrno>
#include <cstdio>
#include <cstring>
#include <dirent.h>
#include <fcntl.h>
#include <fstream>
#include <iostream>
#include <set>
#include <string>
#include <unistd.h>
extern char **environ;
int main() {
  std::set<std::string> allowed = {%s};
  for (char **e = environ; *e; e++) {
    std::string kv = *e;
    std::string name = kv.substr(0, kv.find('='));
    if (!allowed.count(name)) std::cout << "ESCAPED: environment variable " << name << "\n";
  }
  std::ifstream status("/proc/self/status");
  if (!status) std::cout << "ESCAPED: can't read /proc/self/status\n";
  std::set<std::string> seen;
  for (std::string line; std::getline(status, line);) {
    auto i = line.find(":\t");
    if (i == std::string::npos) continue;
    std::string key = line.substr(0, i), value = line.substr(i + 2);
    seen.insert(key);
    if (key.rfind("Cap", 0) == 0 && std::stoull(value, nullptr, 16) != 0) std::cout << "ESCAPED: capabilities " << key << "\n";
    if (key == "NoNewPrivs" && value != "1") std::cout << "ESCAPED: no_new_privs is off\n";
    if (key == "Seccomp" && value != "2") std::cout << "ESCAPED: no seccomp filter\n";
  }
  for (const char *key : {"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb", "NoNewPrivs", "Seccomp"})
    if (!seen.count(key)) std::cout << "ESCAPED: /proc/self/status has no " << key << "\n";
  for (const char *path : {%s}) {
    int fd = open(path, O_WRONLY | O_CREAT, 0644);
    if (fd >= 0) { std::cout << "ESCAPED: wrote " << path << "\n"; close(fd); }
    else if (errno != EROFS) std::cout << "ESCAPED: only " << strerror(errno) << " stopped " << path << "\n";
  }
  if (DIR *d = opendir("/sys/class/net")) {
    while (dirent *e = readdir(d)) {
      std::string n = e->d_name;
      if (n != "." && n != ".." && n != "lo") std::cout << "ESCAPED: a network interface " << n << "\n";
    }
    closedir(d);
  } else std::cout << "ESCAPED: can't list /sys/class/net\n";
  std::cout << "DONE" << std::endl;
}
`

const cppHog = `
#include <cstdlib>
#include <cstring>
#include <iostream>
int main() {
  int n = 0;
  for (; n < %d; n++) { char *b = (char *) malloc(64 << 20); if (!b) break; memset(b, 1, 64 << 20); }
  std::cout << "ESCAPED: held " << n * 64 << " MB" << std::endl;
}
`

const goProbe = `package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func main() {
	allowed := map[string]bool{}
	for _, n := range []string{%s} {
		allowed[n] = true
	}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !allowed[name] {
			fmt.Println("ESCAPED: environment variable", name)
		}
	}
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		fmt.Println("ESCAPED: can't read /proc/self/status:", err)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(line, ":\t")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		seen[key] = true
		if strings.HasPrefix(key, "Cap") {
			if v, _ := strconv.ParseUint(value, 16, 64); v != 0 {
				fmt.Println("ESCAPED: capabilities", key)
			}
		}
		if key == "NoNewPrivs" && value != "1" {
			fmt.Println("ESCAPED: no_new_privs is off")
		}
		if key == "Seccomp" && value != "2" {
			fmt.Println("ESCAPED: no seccomp filter")
		}
	}
	for _, key := range []string{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb", "NoNewPrivs", "Seccomp"} {
		if !seen[key] {
			fmt.Println("ESCAPED: /proc/self/status has no", key)
		}
	}
	for _, path := range []string{%s} {
		err := os.WriteFile(path, []byte("x"), 0o644)
		if err == nil {
			fmt.Println("ESCAPED: wrote", path)
		} else if !errors.Is(err, syscall.EROFS) {
			fmt.Println("ESCAPED: only", err, "stopped", path)
		}
	}
	nics, err := os.ReadDir("/sys/class/net")
	if err != nil {
		fmt.Println("ESCAPED: can't list /sys/class/net:", err)
	}
	for _, n := range nics {
		if n.Name() != "lo" {
			fmt.Println("ESCAPED: a network interface", n.Name())
		}
	}
	fmt.Println("DONE")
}
`

const goHog = `package main

import "fmt"

func main() {
	var held [][]byte
	for i := 0; i < %d; i++ {
		b := make([]byte, 64<<20)
		for j := range b {
			b[j] = 1
		}
		held = append(held, b)
	}
	fmt.Println("ESCAPED: held", len(held)*64, "MB")
}
`

// Native gives each other language's probe and hog. chunks is how many 64 MB pieces
// the hog asks for: four times the memory the run is allowed, toolchain included. Java
// has no hog: its heap is sized to the job (-Xmx), so a heap hog only ever meets the
// JVM's own limit (scored as a memory limit) and proves nothing about the container's.
func Native(memoryMB int) []Attack {
	chunks := func(lang string) int {
		l, _ := engine.Lookup(lang)
		return (memoryMB + l.Overhead.MemoryMB) * 4 / 64
	}
	var out []Attack
	for _, lang := range []string{"javascript", "typescript"} {
		out = append(out,
			Attack{Name: lang + ": probe", Language: lang, Allowed: []string{ok}, CatchesWeak: true,
				Source: fmt.Sprintf(jsProbe, quoted(imageEnv[lang]), quoted(writePaths))},
			Attack{Name: lang + ": memory", Language: lang, Resource: true, Allowed: []string{memory},
				Source: fmt.Sprintf(jsHog, chunks(lang))})
	}
	return append(out,
		Attack{Name: "java: probe", Language: "java", Allowed: []string{ok}, CatchesWeak: true,
			Source: fmt.Sprintf(javaProbe, quoted(imageEnv["java"]), quoted(writePaths))},
		Attack{Name: "cpp: probe", Language: "cpp", Allowed: []string{ok}, CatchesWeak: true,
			Source: fmt.Sprintf(cppProbe, quoted(imageEnv["cpp"]), quoted(writePaths))},
		Attack{Name: "cpp: memory", Language: "cpp", Resource: true, Allowed: []string{memory},
			Source: fmt.Sprintf(cppHog, chunks("cpp"))},
		Attack{Name: "go: probe", Language: "go", Allowed: []string{ok}, CatchesWeak: true,
			Source: fmt.Sprintf(goProbe, quoted(imageEnv["go"]), quoted(writePaths))},
		Attack{Name: "go: memory", Language: "go", Resource: true, Allowed: []string{memory},
			Source: fmt.Sprintf(goHog, chunks("go"))},
	)
}
