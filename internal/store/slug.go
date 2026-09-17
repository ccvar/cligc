package store

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
	"unicode"
)

// Slugify 把标题转成 URL 片段。
//
// 中文标题转拼音需要词典，这里刻意不做：非 ASCII 字符直接丢弃，如果剩下的
// 部分太短，就退化成一个随机短码。用户可以在编辑界面显式覆盖 slug。
// 这比塞一个百分号编码的中文 URL 更好维护，也避免了引入词典依赖。
func Slugify(title string) string {
	if s := slugifyASCII(title); len(s) >= 2 {
		return s
	}
	return RandomSlug()
}

// TagSlug 和 Slugify 的区别在于回退策略：标签大量是纯中文（"Go语言"、"随笔"），
// 退化成随机短码会让 URL 完全不可读，所以这里保留原文小写形式，
// 由模板负责百分号编码。标签页本来就输出 noindex，URL 好看比好收录重要。
func TagSlug(name string) string {
	if s := slugifyASCII(name); s != "" {
		return s
	}
	return strings.ToLower(strings.TrimSpace(name))
}

// slugifyASCII 只保留 ASCII 字母数字，其余压成连字符；无回退。
func slugifyASCII(title string) string {
	var sb strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			sb.WriteRune(r)
			dash = false
		default:
			if !dash && sb.Len() > 0 {
				sb.WriteByte('-')
				dash = true
			}
		}
	}
	s := strings.Trim(sb.String(), "-")
	if len(s) > 60 {
		s = strings.Trim(s[:60], "-")
	}
	return s
}

var b32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// RandomSlug 生成一个 8 字符的小写随机短码。
func RandomSlug() string {
	b := make([]byte, 5)
	rand.Read(b)
	return b32.EncodeToString(b)
}
