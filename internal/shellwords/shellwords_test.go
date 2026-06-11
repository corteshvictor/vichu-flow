package shellwords

import (
	"reflect"
	"testing"
)

func TestSplit(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"pytest", []string{"pytest"}},
		{"go test ./...", []string{"go", "test", "./..."}},
		{`pytest -k "not slow"`, []string{"pytest", "-k", "not slow"}},
		{`sh -c 'sleep 2; echo done'`, []string{"sh", "-c", "sleep 2; echo done"}},
		{`echo "it's fine"`, []string{"echo", "it's fine"}},
		{`cmd /c if exist src\feature.txt`, []string{"cmd", "/c", "if", "exist", `src\feature.txt`}},
		{"  a\tb   c ", []string{"a", "b", "c"}},
		{`foo ""`, []string{"foo", ""}},
		{"", nil},
	}
	for _, c := range cases {
		if got := Split(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Split(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestSplitScript(t *testing.T) {
	cases := []struct {
		in   string
		want [][]string
	}{
		{"rm -rf build", [][]string{{"rm", "-rf", "build"}}},
		{"echo hi && rm -rf build", [][]string{{"echo", "hi"}, {"rm", "-rf", "build"}}},
		{"make; git push", [][]string{{"make"}, {"git", "push"}}},
		{"a | b || c", [][]string{{"a"}, {"b"}, {"c"}}},
		// Operators inside quotes are not boundaries.
		{`echo "a; b" && ls`, [][]string{{"echo", "a; b"}, {"ls"}}},
	}
	for _, c := range cases {
		if got := SplitScript(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("SplitScript(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}
