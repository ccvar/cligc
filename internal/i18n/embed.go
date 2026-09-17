package i18n

import "embed"

//go:embed locales/*.json
var builtin embed.FS

// DefaultCode 是内置目录里作为基准的语言。覆盖率都是相对它算的。
const DefaultCode = "zh-Hans"

// LoadBuiltin 加载编译进二进制的那几份目录。
func LoadBuiltin() error { return LoadFS(builtin, "locales", DefaultCode) }
