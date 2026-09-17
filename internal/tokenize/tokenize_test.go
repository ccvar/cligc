package tokenize

import "testing"

func TestIndex(t *testing.T) {
	cases := []struct{ in, want string }{
		{"中文文章", "中文 文文 文章"},
		{"Go 和 SQLite", "go 和 sqlite"},
		{"用 Go 写博客", "用 go 写博 博客"},
		{"", ""},
		{"!!!???", ""},
		{"a", "a"},
		{"中", "中"},
	}
	for _, c := range cases {
		if got := Index(c.in); got != c.want {
			t.Errorf("Index(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestQuery(t *testing.T) {
	cases := []struct{ in, want string }{
		{"中文文章", `"中文 文文 文章"`},
		{"go sqlite", `"go" AND "sqlite"`},
		{"用 Go 写", `"用" AND "go" AND "写"`},
		{"   ", ""},
		{`he"llo`, `"he" AND "llo"`},
	}
	for _, c := range cases {
		if got := Query(c.in); got != c.want {
			t.Errorf("Query(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
