package engine

import (
	"regexp"
	"strings"
)

// Interval's Q-WD-17: why a run that didn't finish failed, said for a test its player
// may see (a visible one), from the toolchain's own output on stderr: the error, and
// the 1-based line of the player's file it names. stderr is the run's own (the
// player's prints too), so this is never asked for a hidden test (LLDQ-4).

var verdictWords = map[Verdict]string{
	TimeLimit:        "time limit: it ran too long",
	MemoryLimit:      "memory limit: it ran out of memory",
	OutputLimit:      "output limit: it printed too much",
	CompileTimeLimit: "compile time limit: it took too long to compile",
}

// Where a toolchain names a line of a file: Python's traceback lists the innermost frame
// last; the others print the first error, or the innermost frame, first. g++'s notes
// and warnings name lines too, so only its errors count.
func where(language, file string) (re *regexp.Regexp, innermostLast bool) {
	name := regexp.QuoteMeta(file)
	switch language {
	case "python":
		return regexp.MustCompile(`File "(?:[^"\n]*/)?` + name + `", line (\d+)`), true
	case "cpp":
		return regexp.MustCompile(`(?m)(?:^|[\s/])` + name + `:(\d+):\d+: (?:fatal )?error`), false
	}
	return regexp.MustCompile(`(?m)(?:^|[\s(/])` + name + `:(\d+)`), false
}

// The error's own line: Python ends its traceback with it; a compiler or the other
// runtimes say it early, after any location.
var headline = regexp.MustCompile(`^(?:panic: |fatal error: |Exception in thread |[A-Za-z.]*(?:Error|Exception)\b)`)

// Diagnose says why a run with verdict v failed, and the line of the player's file
// (file; "" when the job has none of its own) the toolchain's output names, 0 when it
// names none. The message isn't cleaned: the caller cleans what it returns.
func Diagnose(language string, v Verdict, stderr, file string) (string, int) {
	if words, ok := verdictWords[v]; ok {
		return words, 0
	}
	prefix := "runtime error"
	if v == CompileError {
		prefix = "compile error"
	}
	lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
	line, at := 0, -1
	if file != "" {
		re, last := where(language, file)
		matches := re.FindAllStringSubmatchIndex(stderr, -1)
		if len(matches) > 0 {
			m := matches[0]
			if last {
				m = matches[len(matches)-1]
			}
			line = atoi(stderr[m[2]:m[3]])
			at = strings.Count(stderr[:m[0]], "\n")
			if m[0] < len(stderr) && stderr[m[0]] == '\n' {
				at++
			}
		}
	}
	text := ""
	switch {
	case v == CompileError && at >= 0 && at < len(lines):
		// The compiler's own error line, without the location it starts with.
		text = strings.TrimSpace(lines[at])
		if _, rest, ok := strings.Cut(text, file+":"); ok {
			text = strings.TrimLeft(strings.TrimLeft(rest, "0123456789:"), " ")
		}
	case v == CompileError:
		for _, l := range lines {
			l = strings.TrimSpace(l)
			if l != "" && !strings.HasPrefix(l, "#") && !strings.HasPrefix(l, "In file included from") {
				text = l
				break
			}
		}
	case language == "python":
		for i := len(lines) - 1; i >= 0 && text == ""; i-- {
			text = strings.TrimSpace(lines[i])
		}
	default:
		for _, l := range lines {
			if headline.MatchString(l) {
				text = strings.TrimSpace(l)
				break
			}
		}
	}
	if text == "" {
		if v == CompileError {
			return prefix, line
		}
		return prefix + ": the program crashed", line
	}
	return prefix + ": " + text, line
}

// A line number as the toolchain printed it; past the contract's bound it says nothing.
func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
		if n > 1_000_000 {
			return 0
		}
	}
	return n
}
