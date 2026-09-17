// Package i18n 提供界面文案的多语言支持。
//
// 两个集合要分清楚：
//
//   - **内容语言**：文章用哪种语言写。任何 BCP 47 标签都行，见 langs.go 的
//     注册表。加一种内容语言不需要任何翻译工作。
//   - **界面语言**：界面文案翻成了哪几种。只有加载到目录文件、且覆盖率达标
//     的语言才会出现在切换器里。
//
// 混淆这两者会得出"支持 50 种语言要写一万条文案"的结论——实际上内容侧
// 一条都不用写。
//
// 文案存 JSON 数据文件而不是 Go 源码：加一种界面语言 = 丢一个文件进去，
// 不用改代码、不用重新编译（配合 -lang-dir 连重启都只需要一次）。
package i18n

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/text/feature/plural"
	"golang.org/x/text/language"
)

// MinCoverage 是一种语言出现在切换器里所需的最低翻译覆盖率。
//
// 门槛存在的理由：用户选了「Español」结果看到一半中文，比根本没有西班牙语
// 选项更糟——他会以为站点坏了。没达标的目录仍然会加载并逐条生效
// （通过 ?lang= 或直接访问前缀时），只是不主动推荐。
const MinCoverage = 0.8

// Msg 是一条文案，按 CLDR 的六种复数类别存。
//
// 绝大多数文案只有 Other。只有真正随数量变形的句子才需要填其它字段，
// 而具体需要哪几个由语言决定：英文两个、俄语四个、阿拉伯语六个。
type Msg struct {
	Zero  string `json:"zero,omitempty"`
	One   string `json:"one,omitempty"`
	Two   string `json:"two,omitempty"`
	Few   string `json:"few,omitempty"`
	Many  string `json:"many,omitempty"`
	Other string `json:"other"`
}

// UnmarshalJSON 允许两种写法：裸字符串（只有 Other）或对象（多种形式）。
// 九成以上的条目没有复数，逼着它们全写成 {"other": "..."} 只会让文件难读。
func (m *Msg) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &m.Other)
	}
	type raw Msg
	return json.Unmarshal(b, (*raw)(m))
}

// form 按 CLDR 类别取对应形式，缺失则回退到 Other。
func (m Msg) form(f plural.Form) string {
	var s string
	switch f {
	case plural.Zero:
		s = m.Zero
	case plural.One:
		s = m.One
	case plural.Two:
		s = m.Two
	case plural.Few:
		s = m.Few
	case plural.Many:
		s = m.Many
	}
	if s == "" {
		return m.Other
	}
	return s
}

// Catalog 是一个语言的全部文案。
type Catalog map[string]Msg

// Lang 是一个界面语言。
type Lang struct {
	Code     string  // BCP 47
	Prefix   string  // URL 前缀；默认语言为空
	Name     string  // 母语名
	Dir      string  // ltr / rtl
	Coverage float64 // 相对默认语言的翻译覆盖率
	tag      language.Tag
}

// Ready 报告这个语言是否翻译得足够完整，可以出现在切换器里。
func (l Lang) Ready() bool { return l.Coverage >= MinCoverage }

var (
	mu          sync.RWMutex
	catalogs    = map[string]Catalog{}
	langs       = map[string]Lang{}
	order       []string // 按加载顺序，第一个是默认语言
	defaultCode string
)

// locale 是目录文件的结构。
type locale struct {
	Lang     string  `json:"lang"`
	Name     string  `json:"name"`
	Messages Catalog `json:"messages"`
}

// Load 读入一个目录文件。第一个被加载的语言成为默认语言。
//
// 重复加载同一语言会**合并**而不是覆盖：内置目录先加载，磁盘上的
// -lang-dir 随后加载，于是外部文件可以只写想改的那几条，其余沿用内置。
func Load(data []byte) error {
	var l locale
	if err := json.Unmarshal(data, &l); err != nil {
		return err
	}
	if l.Lang == "" {
		return errors.New("locale file has no \"lang\"")
	}
	tag, err := language.Parse(l.Lang)
	if err != nil {
		return fmt.Errorf("%q is not a valid BCP 47 tag: %w", l.Lang, err)
	}

	mu.Lock()
	defer mu.Unlock()

	dst, ok := catalogs[l.Lang]
	if !ok {
		dst = Catalog{}
		catalogs[l.Lang] = dst
		order = append(order, l.Lang)
		if defaultCode == "" {
			defaultCode = l.Lang
		}
	}
	for k, v := range l.Messages {
		dst[k] = v
	}

	name := l.Name
	if name == "" {
		name = Name(l.Lang)
	}
	prefix := ""
	if l.Lang != defaultCode {
		prefix = urlPrefix(l.Lang)
	}
	langs[l.Lang] = Lang{Code: l.Lang, Prefix: prefix, Name: name, Dir: Dir(l.Lang), tag: tag}
	recalc()
	return nil
}

// urlPrefix 把语言标签变成 URL 前缀。
//
// 用小写的完整标签而不是只取基础语言：zh-Hans 和 zh-Hant 必须是两个不同的
// 前缀，只取 "zh" 会让它们撞在一起。
func urlPrefix(code string) string { return strings.ToLower(code) }

// recalc 重算每个语言相对默认语言的覆盖率。调用方必须已持有写锁。
func recalc() {
	base := catalogs[defaultCode]
	total := 0
	for k := range base {
		if !OptionalKeys[k] {
			total++
		}
	}
	for code, c := range catalogs {
		n := 0
		for k := range base {
			if OptionalKeys[k] {
				continue
			}
			m, ok := c[k]
			if !ok || m.Other == "" {
				continue
			}
			// 值和默认语言**完全相同**算作没翻。
			//
			// `cligc lang new` 生成模板时把原文填进去当占位（让翻译的人
			// 看得到要翻什么），只看"有没有值"的话，一个一条没动的模板
			// 会显示成 100% 覆盖——那个数字比没有还坏，它会让人以为
			// 这门语言已经好了。
			//
			// 代价是少数确实无需改动的词（RSS、API Token）会被算成未翻，
			// 但 80% 的门槛足以吸收这点误差。
			if code != defaultCode && m.Other == base[k].Other {
				continue
			}
			n++
		}
		l := langs[code]
		if total == 0 {
			l.Coverage = 1
		} else {
			l.Coverage = float64(n) / float64(total)
		}
		langs[code] = l
	}
}

// LoadFS 加载一个目录下的全部 *.json。defaultFirst 指定哪个文件先加载
// （它决定默认语言），其余按文件名排序。
func LoadFS(fsys fs.FS, dir, defaultFirst string) error {
	entries, err := fs.Glob(fsys, dir+"/*.json")
	if err != nil {
		return err
	}
	sort.Strings(entries)
	// 默认语言必须第一个加载：覆盖率是拿它当基准算的
	sort.SliceStable(entries, func(i, j int) bool {
		return filepath.Base(entries[i]) == defaultFirst+".json"
	})
	for _, e := range entries {
		b, err := fs.ReadFile(fsys, e)
		if err != nil {
			return err
		}
		if err := Load(b); err != nil {
			return fmt.Errorf("%s: %w", e, err)
		}
	}
	return nil
}

// LoadDir 加载磁盘上的目录文件，用于在不重新编译的前提下增删语言。
//
// 目录不存在不算错误：绝大多数部署不会用到它，为此让服务起不来是本末倒置。
func LoadDir(dir string) (int, error) {
	if dir == "" {
		return 0, nil
	}
	entries, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(entries) == 0 {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	sort.Strings(entries)
	n := 0
	for _, e := range entries {
		b, err := os.ReadFile(e)
		if err != nil {
			return n, err
		}
		if err := Load(b); err != nil {
			return n, fmt.Errorf("%s: %w", e, err)
		}
		n++
	}
	return n, nil
}

// Default 是默认语言。
func Default() Lang {
	mu.RLock()
	defer mu.RUnlock()
	return langs[defaultCode]
}

// Languages 返回全部已加载的界面语言，默认语言在最前，其余按母语名排序。
func Languages() []Lang {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Lang, 0, len(langs))
	for _, c := range order {
		if c == defaultCode {
			out = append(out, langs[c])
		}
	}
	rest := make([]Lang, 0, len(langs))
	for c, l := range langs {
		if c != defaultCode {
			rest = append(rest, l)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].Name < rest[j].Name })
	return append(out, rest...)
}

// ReadyLanguages 返回可以出现在切换器里的语言（覆盖率达标的）。
func ReadyLanguages() []Lang {
	out := []Lang{}
	for _, l := range Languages() {
		if l.Ready() {
			out = append(out, l)
		}
	}
	return out
}

// ByCode 按标签查找已加载的界面语言。
func ByCode(code string) (Lang, bool) {
	mu.RLock()
	defer mu.RUnlock()
	l, ok := langs[code]
	return l, ok
}

// ByPrefix 按 URL 前缀查找。空前缀对应默认语言。
func ByPrefix(prefix string) (Lang, bool) {
	if prefix == "" {
		return Default(), true
	}
	mu.RLock()
	defer mu.RUnlock()
	for _, l := range langs {
		if l.Prefix != "" && l.Prefix == prefix {
			return l, true
		}
	}
	return Lang{}, false
}

// Codes 返回全部已加载的界面语言标签。
func Codes() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(langs))
	for c := range langs {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// OptionalKeys 是允许只在部分语言里出现的 key，不计入覆盖率。
var OptionalKeys = map[string]bool{
	"site.title":       true,
	"site.description": true,
	// 品牌副名只有汉字圈有（轻格 / 輕格 / 軽格 / 경격）。拉丁、西里尔、
	// 阿拉伯文里把一个生造的拉丁名转写过去只会得到一串没有意义的音节，
	// 所以这些语言只出 cligc 字标本身——留空是设计结果，不是没翻。
	"brand.companion": true,
}

// lookup 找一条文案，找不到则退回默认语言。
//
// 回退是**逐条**的：一个翻了 60% 的语言，那 60% 照常显示，剩下的用默认语言，
// 而不是整个语言不可用。50 种界面语言的现实就是它们的完成度参差不齐。
func lookup(code, key string) (Msg, bool) {
	mu.RLock()
	defer mu.RUnlock()
	if m, ok := catalogs[code][key]; ok && m.Other != "" {
		return m, true
	}
	if code != defaultCode {
		if m, ok := catalogs[defaultCode][key]; ok && m.Other != "" {
			return m, true
		}
	}
	return Msg{}, false
}

// T 取一条文案。找不到时返回 key 本身——空串会让按钮变成一块空白，
// 反而比刺眼的 "nav.admin" 更难发现。
func T(code, key string) string {
	if m, ok := lookup(code, key); ok {
		return m.Other
	}
	return key
}

// Own 取这个语言自己的那一条，不回退到默认语言。
//
// 逐条回退对界面文案是对的：缺一句就用默认语言那句，总比空白强。
// 但有些 key 的「没有」本身就是内容——比如品牌副名，汉字圈有、拉丁语种
// 故意留空。那种 key 一旦回退，德语页面就会挂上一个中文副名。
func Own(code, key string) (string, bool) {
	mu.RLock()
	defer mu.RUnlock()
	m, ok := catalogs[code][key]
	if !ok || m.Other == "" {
		return "", false
	}
	return m.Other, true
}

// N 取一条带数量的文案，按 CLDR 规则选形式，并把 {n} 换成数字。
//
// 复数类别由 x/text 的 CLDR 数据决定。自己写"n==1 用单数"那套只对英文成立：
// 俄语有四类、阿拉伯语有六类，而 0 在英文里是复数、在法语里是单数。
// 这些规则没有一个是能靠直觉猜对的。
func N(code, key string, n int) string {
	m, ok := lookup(code, key)
	if !ok {
		return key
	}
	tag := language.Und
	if l, ok := ByCode(code); ok {
		tag = l.tag
	}
	s := m.form(plural.Cardinal.MatchPlural(tag, n, 0, 0, 0, 0))
	if s == "" {
		return key
	}
	return strings.ReplaceAll(s, "{n}", strconv.Itoa(n))
}

// F 取一条文案并按顺序替换 {0} {1}…
func F(code, key string, args ...string) string {
	s := T(code, key)
	for i, a := range args {
		s = strings.ReplaceAll(s, "{"+strconv.Itoa(i)+"}", a)
	}
	return s
}

// Missing 返回某语言相对默认语言缺失的 key。
func Missing(code string) []string {
	mu.RLock()
	defer mu.RUnlock()
	var out []string
	for k := range catalogs[defaultCode] {
		if OptionalKeys[k] {
			continue
		}
		if m, ok := catalogs[code][k]; !ok || m.Other == "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Untranslated 返回值和默认语言完全相同的 key。
//
// 和 Missing 是两回事，必须分开报：
//
//   - Missing 是硬缺陷——key 根本不在目录里，界面上会显示成默认语言。
//   - Untranslated 是软信号——值照抄了原文。多数情况确实是没翻，
//     但也有合法同文的（"Slug"、"RSS"、"API Token" 在很多语言里就该一样）。
//
// 把两者混成一个数字，要么会把合法同文报成缺陷，要么会让一个一条没动的
// 模板显示成 100% 完成。覆盖率用软口径（它是进度指示），
// 完整性检查用硬口径（它是 bug 检查）。
func Untranslated(code string) []string {
	mu.RLock()
	defer mu.RUnlock()
	if code == defaultCode {
		return nil
	}
	base := catalogs[defaultCode]
	var out []string
	for k, bm := range base {
		if OptionalKeys[k] {
			continue
		}
		if m, ok := catalogs[code][k]; ok && m.Other != "" && m.Other == bm.Other {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Keys 返回默认语言目录里的全部 key。
func Keys() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(catalogs[defaultCode]))
	for k := range catalogs[defaultCode] {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Reset 清空所有已加载的内容，仅供测试使用。
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	catalogs = map[string]Catalog{}
	langs = map[string]Lang{}
	order = nil
	defaultCode = ""
}
