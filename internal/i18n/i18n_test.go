package i18n

import (
	"encoding/json"
	"strings"
	"testing"
)

func load(t *testing.T) {
	t.Helper()
	Reset()
	if err := LoadBuiltin(); err != nil {
		t.Fatal(err)
	}
}

// TestBuiltinLanguagesAreComplete 内置的几种语言必须 100% 完整。
//
// 覆盖率门槛是给**外部添加**的语言用的——它们的完成度参差不齐是常态。
// 但随二进制一起发的这几份，漏一条就是 bug。
func TestBuiltinLanguagesAreComplete(t *testing.T) {
	load(t)
	for _, l := range Languages() {
		if miss := Missing(l.Code); len(miss) > 0 {
			t.Errorf("内置目录 %s 缺 %d 条：\n  %s", l.Code, len(miss), strings.Join(miss[:min(5, len(miss))], "\n  "))
		}
		if !l.Ready() {
			t.Errorf("%s 覆盖率 %.2f，内置目录不该低于门槛", l.Code, l.Coverage)
		}
	}
}

// TestCLDRPlurals 复数按 CLDR 规则选形式。
//
// 自己写"n==1 用单数"那套只对英文成立。这里挑的几个例子都是直觉会猜错的：
// 英文的 0 是复数、法语的 0 是单数、俄语的 2 和 5 是两种不同形式。
func TestCLDRPlurals(t *testing.T) {
	load(t)
	if err := Load([]byte(`{"lang":"ru","name":"Русский","messages":{
		"common.count":{"one":"{n} статья","few":"{n} статьи","many":"{n} статей","other":"{n} статьи"}}}`)); err != nil {
		t.Fatal(err)
	}
	if err := Load([]byte(`{"lang":"fr","name":"Français","messages":{
		"common.count":{"one":"{n} article","other":"{n} articles"}}}`)); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		code string
		n    int
		want string
	}{
		{"en", 0, "0 posts"}, // 英文的 0 是复数
		{"en", 1, "1 post"},
		{"en", 2, "2 posts"},
		{"fr", 0, "0 article"}, // 法语的 0 是单数
		{"fr", 1, "1 article"},
		{"fr", 2, "2 articles"},
		{"ru", 1, "1 статья"},   // 俄语 one
		{"ru", 2, "2 статьи"},   // few
		{"ru", 5, "5 статей"},   // many
		{"zh-Hans", 1, "共 1 篇"}, // 中文无复数变化
		{"zh-Hans", 9, "共 9 篇"},
	}
	for _, c := range cases {
		if got := N(c.code, "common.count", c.n); got != c.want {
			t.Errorf("N(%s, %d) = %q, want %q", c.code, c.n, got, c.want)
		}
	}
}

// TestPartialLanguageFallsBackPerKey 一个翻了一半的语言，翻了的部分照常显示，
// 没翻的退回默认语言——而不是整个语言不可用。
// 50 种界面语言的现实就是完成度参差不齐。
func TestPartialLanguageFallsBackPerKey(t *testing.T) {
	load(t)
	if err := Load([]byte(`{"lang":"sw","name":"Kiswahili","messages":{"nav.admin":"Msimamizi"}}`)); err != nil {
		t.Fatal(err)
	}
	if got := T("sw", "nav.admin"); got != "Msimamizi" {
		t.Errorf("翻译过的 key = %q", got)
	}
	if got := T("sw", "nav.login"); got != T(DefaultCode, "nav.login") {
		t.Errorf("未翻译的 key = %q，应退回默认语言", got)
	}
	// 覆盖率太低，不该出现在切换器里
	l, _ := ByCode("sw")
	if l.Ready() {
		t.Errorf("覆盖率 %.2f 的语言不该上架", l.Coverage)
	}
	for _, r := range ReadyLanguages() {
		if r.Code == "sw" {
			t.Error("未达标的语言出现在了切换器里")
		}
	}
}

// TestLoadMergesInsteadOfReplacing 外部目录可以只写想改的几条。
func TestLoadMergesInsteadOfReplacing(t *testing.T) {
	load(t)
	before := T("en", "nav.login")
	if err := Load([]byte(`{"lang":"en","messages":{"nav.admin":"Dashboard"}}`)); err != nil {
		t.Fatal(err)
	}
	if got := T("en", "nav.admin"); got != "Dashboard" {
		t.Errorf("覆盖失败: %q", got)
	}
	if got := T("en", "nav.login"); got != before {
		t.Errorf("没被覆盖的 key 丢了: %q -> %q", before, got)
	}
}

func TestRTLAndNames(t *testing.T) {
	load(t)
	// 忘了标 RTL 的话，这些语言的整个界面会是镜像错乱的
	for _, c := range []string{"ar", "he", "fa", "ur"} {
		if Dir(c) != "rtl" {
			t.Errorf("%s 应当是 rtl", c)
		}
	}
	if Dir("ja") != "ltr" {
		t.Error("ja 应当是 ltr")
	}
	// 母语名：一个只会韩语的人得能在下拉里认出来
	if Name("ko") != "한국어" {
		t.Errorf("ko = %q", Name("ko"))
	}
	if Name("xx-unknown") != "xx-unknown" {
		t.Error("未登记的标签应原样返回，不该被吞掉")
	}
}

func TestPrefixDistinguishesScripts(t *testing.T) {
	load(t)
	if err := Load([]byte(`{"lang":"zh-Hant","name":"繁體中文","messages":{"nav.admin":"管理"}}`)); err != nil {
		t.Fatal(err)
	}
	l, ok := ByCode("zh-Hant")
	if !ok || l.Prefix != "zh-hant" {
		t.Errorf("zh-Hant 前缀 = %q，只取 \"zh\" 会和简体撞车", l.Prefix)
	}
	if d := Default(); d.Prefix != "" {
		t.Error("默认语言不该有前缀，否则 / 会变成重定向")
	}
}

func TestInvalidTagRejected(t *testing.T) {
	load(t)
	if err := Load([]byte(`{"lang":"not a tag!","messages":{}}`)); err == nil {
		t.Error("非法 BCP 47 标签应当报错，而不是悄悄加载一个用不了的语言")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestCoverageIgnoresUntranslatedPlaceholders 守住覆盖率的诚实性。
//
// `cligc lang new` 生成模板时把原文填进去当占位（让翻译的人看得到要翻什么）。
// 只看"有没有值"的话，一个一条没动的模板会显示成 100% 覆盖——那个数字比
// 没有还坏，它会让人以为这门语言已经可以上线了。
func TestCoverageIgnoresUntranslatedPlaceholders(t *testing.T) {
	load(t)
	// 造一份"原样复制默认语言"的目录，模拟一条没翻的模板
	msgs := map[string]any{}
	for _, k := range Keys() {
		msgs[k] = T(DefaultCode, k)
	}
	b, _ := json.Marshal(map[string]any{"lang": "ja", "name": "日本語", "messages": msgs})
	if err := Load(b); err != nil {
		t.Fatal(err)
	}
	l, _ := ByCode("ja")
	if l.Coverage > 0.05 {
		t.Errorf("一条未翻的模板算出 %.0f%% 覆盖率——这个数字会让人以为它能上线了", l.Coverage*100)
	}
	if l.Ready() {
		t.Error("未翻译的模板不该出现在切换器里")
	}
	// Missing 是硬缺陷（key 都在，所以为 0）；Untranslated 是软信号
	if n := len(Missing("ja")); n != 0 {
		t.Errorf("Missing = %d，模板里每个 key 都在，硬缺陷应为 0", n)
	}
	if n := len(Untranslated("ja")); n < len(Keys())/2 {
		t.Errorf("Untranslated = %d，一条没翻的模板应当几乎全中", n)
	}

	// 真翻了几条之后，覆盖率要跟着动
	if err := Load([]byte(`{"lang":"ja","messages":{"nav.admin":"管理者","nav.login":"ログイン"}}`)); err != nil {
		t.Fatal(err)
	}
	if l2, _ := ByCode("ja"); l2.Coverage <= l.Coverage {
		t.Error("翻译了条目之后覆盖率没有上升")
	}
}
