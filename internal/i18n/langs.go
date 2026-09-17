package i18n

// 语言注册表：BCP 47 标签 -> 母语名 + 书写方向。
//
// 这张表只回答"这个标签叫什么、往哪边写"，和有没有界面翻译无关。
// 一篇文章可以标成表里任何一种语言——内容语言是无限的，翻译不是前提。
// 界面语言则只有加载到目录文件的那几种，两者是不同的集合。
//
// 母语名用该语言自己的文字写：一个只会韩语的人在下拉里找得到「한국어」，
// 找不到「Korean」。
var registry = map[string]struct {
	Name string
	RTL  bool
}{
	"zh-Hans": {"简体中文", false}, "zh-Hant": {"繁體中文", false},
	"en": {"English", false}, "ja": {"日本語", false}, "ko": {"한국어", false},
	"es": {"Español", false}, "pt": {"Português", false}, "pt-BR": {"Português (Brasil)", false},
	"fr": {"Français", false}, "de": {"Deutsch", false}, "it": {"Italiano", false},
	"ru": {"Русский", false}, "uk": {"Українська", false}, "pl": {"Polski", false},
	"cs": {"Čeština", false}, "sk": {"Slovenčina", false}, "hu": {"Magyar", false},
	"ro": {"Română", false}, "bg": {"Български", false}, "sr": {"Српски", false},
	"hr": {"Hrvatski", false}, "sl": {"Slovenščina", false}, "el": {"Ελληνικά", false},
	"tr": {"Türkçe", false}, "nl": {"Nederlands", false}, "sv": {"Svenska", false},
	"da": {"Dansk", false}, "nb": {"Norsk bokmål", false}, "fi": {"Suomi", false},
	"is": {"Íslenska", false}, "et": {"Eesti", false}, "lv": {"Latviešu", false},
	"lt": {"Lietuvių", false}, "vi": {"Tiếng Việt", false}, "th": {"ไทย", false},
	"id": {"Bahasa Indonesia", false}, "ms": {"Bahasa Melayu", false},
	"tl": {"Tagalog", false}, "my": {"မြန်မာ", false}, "km": {"ខ្មែរ", false},
	"lo": {"ລາວ", false}, "hi": {"हिन्दी", false}, "bn": {"বাংলা", false},
	"ta": {"தமிழ்", false}, "te": {"తెలుగు", false}, "mr": {"मराठी", false},
	"gu": {"ગુજરાતી", false}, "kn": {"ಕನ್ನಡ", false}, "ml": {"മലയാളം", false},
	"pa": {"ਪੰਜਾਬੀ", false}, "si": {"සිංහල", false}, "ne": {"नेपाली", false},
	"ka": {"ქართული", false}, "hy": {"Հայերեն", false}, "az": {"Azərbaycan", false},
	"kk": {"Қазақша", false}, "uz": {"Oʻzbekcha", false}, "mn": {"Монгол", false},
	"sw": {"Kiswahili", false}, "am": {"አማርኛ", false}, "af": {"Afrikaans", false},
	"ca": {"Català", false}, "gl": {"Galego", false}, "eu": {"Euskara", false},

	// 从右往左书写。忘了标 RTL 的话，这些语言的整个界面会是镜像错乱的，
	// 而且在一个只读中文的人眼里完全看不出问题。
	"ar": {"العربية", true}, "he": {"עברית", true},
	"fa": {"فارسی", true}, "ur": {"اردو", true},
}

// Name 返回语言的母语名；未登记的标签原样返回，不吞掉。
func Name(code string) string {
	if e, ok := registry[code]; ok {
		return e.Name
	}
	return code
}

// Dir 返回书写方向。
func Dir(code string) string {
	if e, ok := registry[code]; ok && e.RTL {
		return "rtl"
	}
	return "ltr"
}

// Known 报告该标签是否在注册表里。
func Known(code string) bool { _, ok := registry[code]; return ok }

// AllCodes 返回注册表里的全部标签，用于后台的内容语言选择器。
func AllCodes() []string {
	out := make([]string, 0, len(registry))
	for c := range registry {
		out = append(out, c)
	}
	return out
}
