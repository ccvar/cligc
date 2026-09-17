package render

import (
	"strings"
	"testing"
)

func TestHighlight(t *testing.T) {
	got := Highlight("这是一篇讲 Go 和 SQLite 的文章，内容很长很长。", []string{"sqlite"}, 20)
	if !strings.Contains(got, "<mark>SQLite</mark>") {
		t.Fatalf("no mark: %q", got)
	}
	got = Highlight("前面一大段无关内容，后面才提到中文检索这件事。", []string{"中文检索"}, 12)
	if !strings.Contains(got, "<mark>中文检索</mark>") {
		t.Fatalf("no cjk mark: %q", got)
	}
	// 未命中时退化为普通摘要，且做转义
	got = Highlight("<script>x</script>", []string{"zzz"}, 40)
	if strings.Contains(got, "<script>") {
		t.Fatalf("not escaped: %q", got)
	}
	// 重叠词条不产生嵌套 mark
	got = Highlight("abcabc", []string{"abc", "bca"}, 20)
	if strings.Count(got, "<mark>") != 1 {
		t.Fatalf("overlap not merged: %q", got)
	}
}
