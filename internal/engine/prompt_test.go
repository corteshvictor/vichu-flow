package engine

import (
	"reflect"
	"testing"
)

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"pytest", []string{"pytest"}},
		{"go test ./...", []string{"go", "test", "./..."}},
		{"ruff check .", []string{"ruff", "check", "."}},
		// Quoted argument with spaces — the case strings.Fields broke.
		{`pytest -k "not slow"`, []string{"pytest", "-k", "not slow"}},
		{`sh -c 'sleep 2; echo done'`, []string{"sh", "-c", "sleep 2; echo done"}},
		// Single quotes inside double quotes and vice versa.
		{`echo "it's fine"`, []string{"echo", "it's fine"}},
		{`echo 'say "hi"'`, []string{"echo", `say "hi"`}},
		// Windows backslash paths must survive (no escape handling).
		{`cmd /c if exist src\feature.txt`, []string{"cmd", "/c", "if", "exist", `src\feature.txt`}},
		// Extra whitespace and tabs collapse.
		{"  a\tb   c ", []string{"a", "b", "c"}},
		// Empty quotes produce an empty argument.
		{`foo ""`, []string{"foo", ""}},
		{"", nil},
	}
	for _, c := range cases {
		got := splitCommand(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitCommand(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}
