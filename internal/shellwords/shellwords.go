// Package shellwords tokenizes command strings with shell-like quoting, and
// splits compound scripts into their constituent commands. It is intentionally
// NOT a shell: no escape characters (backslashes stay literal so Windows paths
// survive), no variable/glob expansion. It exists so both the engine (to build
// argv) and the security policy (to see through shell wrappers like `sh -c`)
// tokenize identically.
package shellwords

import "strings"

// Split tokenizes a command string into argv. Single or double quotes group a
// token and preserve the spaces inside it: `pytest -k "not slow"` →
// ["pytest","-k","not slow"]. argv[0] is the executable.
func Split(s string) []string {
	var args []string
	var cur strings.Builder
	inToken := false
	var quote rune // 0 when not inside quotes, else '\'' or '"'

	flush := func() {
		if inToken {
			args = append(args, cur.String())
			cur.Reset()
			inToken = false
		}
	}

	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inToken = true // even "" yields an (empty) argument
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			cur.WriteRune(r)
			inToken = true
		}
	}
	flush()
	return args
}

// SplitScript splits a (possibly compound) shell script into individual
// commands, cutting at top-level operators (; && || | & and newlines) while
// respecting quotes, then tokenizing each segment. Used by the security policy
// to inspect every command inside a `sh -c '...'` payload.
func SplitScript(s string) [][]string {
	var cmds [][]string
	for _, seg := range splitOnOperators(s) {
		if argv := Split(seg); len(argv) > 0 {
			cmds = append(cmds, argv)
		}
	}
	return cmds
}

// splitOnOperators cuts a script at top-level shell operators, keeping quoted
// spans intact so operators inside quotes are not treated as boundaries.
func splitOnOperators(s string) []string {
	var segs []string
	var cur strings.Builder
	var quote rune
	runes := []rune(s)

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if quote != 0 {
			cur.WriteRune(r)
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
			cur.WriteRune(r)
		case ';', '\n', '\r', '&', '|':
			segs = append(segs, cur.String())
			cur.Reset()
			// Treat && and || as a single boundary.
			if (r == '&' || r == '|') && i+1 < len(runes) && runes[i+1] == r {
				i++
			}
		default:
			cur.WriteRune(r)
		}
	}
	segs = append(segs, cur.String())
	return segs
}
