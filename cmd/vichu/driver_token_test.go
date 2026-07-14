package main

import (
	"bytes"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/corteshvictor/vichu-flow/internal/i18n"
)

// withStdin swaps os.Stdin for a pipe carrying s, for the duration of fn.
func withStdin(t *testing.T, s string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = orig }()
	go func() { _, _ = w.WriteString(s); _ = w.Close() }()
	fn()
}

func TestDriverTokenFromStdin(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	r := driverTokenFlags(fs)
	if err := fs.Parse([]string{"--driver-token-stdin"}); err != nil {
		t.Fatal(err)
	}
	var got string
	withStdin(t, "  secret-token\n", func() {
		tok, err := r.resolve(false)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		got = tok
	})
	if got != "secret-token" {
		t.Fatalf("token from stdin = %q, want trimmed secret-token", got)
	}
}

func TestDriverTokenFromArgvStillWorks(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	r := driverTokenFlags(fs)
	if err := fs.Parse([]string{"--driver-token", "argv-token"}); err != nil {
		t.Fatal(err)
	}
	tok, err := r.resolve(false)
	if err != nil || tok != "argv-token" {
		t.Fatalf("argv token = %q (%v), want argv-token", tok, err)
	}
}

func TestDriverTokenStdinConflictsWithResultStdin(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	r := driverTokenFlags(fs)
	if err := fs.Parse([]string{"--driver-token-stdin"}); err != nil {
		t.Fatal(err)
	}
	_, err := r.resolve(true) // another flag already consumes stdin
	if err == nil || !strings.Contains(err.Error(), "stdin") {
		t.Fatalf("token-stdin + result-stdin must conflict, got %v", err)
	}
}

func TestDriverTokenBothFormsRejected(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	r := driverTokenFlags(fs)
	if err := fs.Parse([]string{"--driver-token", "x", "--driver-token-stdin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.resolve(false); err == nil {
		t.Fatal("passing the token both ways must be rejected")
	}
}

// TestDriverTokenFlagHelpHasNoBogusArgument: `flag.UnquoteUsage` takes the first backtick-
// quoted word in a flag's help as the argument name. The bool --driver-token-stdin must show
// NO argument (it takes none), and --driver-token must show its `token` placeholder — not a
// stray word like "run start" or "ps" leaking from prose backticks.
func TestDriverTokenFlagHelpHasNoBogusArgument(t *testing.T) {
	for _, lang := range []string{"en", "es"} {
		i18n.SetLanguage(lang)
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		driverTokenFlags(fs)
		var buf bytes.Buffer
		fs.SetOutput(&buf)
		fs.PrintDefaults()
		out := buf.String()

		// Only the flag DECLARATION lines (trimmed, they start with a single dash); the wrapped
		// description lines start with prose, so they are skipped.
		for _, line := range strings.Split(out, "\n") {
			decl := strings.TrimSpace(line)
			if decl == "-driver-token-stdin" {
				continue // correct: bool flag, no argument
			}
			if strings.HasPrefix(decl, "-driver-token-stdin ") {
				t.Errorf("[%s] --driver-token-stdin (bool) shows a bogus argument: %q", lang, decl)
			}
			if strings.HasPrefix(decl, "-driver-token ") && decl != "-driver-token token" {
				t.Errorf("[%s] --driver-token should show the `token` placeholder, got: %q", lang, decl)
			}
		}
	}
	i18n.SetLanguage("en")
}
