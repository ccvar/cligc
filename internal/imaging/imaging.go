// Package imaging 把上传的图片收敛成一种格式、一个尺寸上限。
//
// 做这件事的理由不是"WebP 比较新"，是实测出来的账：一张 1600×1000 的
// 照片 JPEG（q85）转成 WebP q80 少 52%，一张真实截图 PNG 少 95%。图片
// 通常是一个内容站里最重的东西，而这个站的整个论点就是轻。
//
// 但转换不是无条件划算的。同一批测试里，一张大片纯色的 PNG 转有损 WebP
// 反而**大了 26%**——有损编码器最不擅长的就是硬边和纯色块。所以这里的
// 规矩是：按来源挑编码方式，编完再跟原图比一次大小，没变小就原样留着。
// 宁可少省一点，也不要出现"上传之后文件变大了"这种解释不通的结果。
package imaging

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"

	"github.com/gen2brain/webp"
	xdraw "golang.org/x/image/draw"
)

// Quality 是有损 WebP 的质量。80 是肉眼几乎看不出差别、体积又明显下来的
// 那个位置；再往上收益迅速衰减（实测 q85 只省 28%，q80 省 52%）。
const Quality = 80

// MaxPixels 是允许解码的像素数上限，约等于 8000×6000。
//
// 这道闸不是为了画质，是为了挡解压炸弹：一个几十 KB 的 PNG 可以声明
// 30000×30000，解出来要 3.6 GB 内存。先读文件头拿尺寸、再决定要不要
// 解码，是唯一能在分配内存之前拦住它的时机。
const MaxPixels = 48_000_000

// ErrTooLarge 表示图片的像素数超过 MaxPixels。
var ErrTooLarge = errors.New("image has too many pixels")

// Result 是处理完的图片。Data 可能就是传进来的原始字节——
// 转换没让它变小时就不换。
type Result struct {
	Data      []byte
	MIME      string
	Width     int
	Height    int
	Resized   bool // 是否因为超过尺寸上限被缩过
	Converted bool // 是否真的换成了 WebP
}

// Process 解码、按需缩放、转成 WebP。maxDim 是长边上限，<=0 表示不缩。
//
// 任何一步不划算或做不了，都退回原图：这个函数的失败模式是"什么都没做"，
// 不是"把用户的图弄坏了"。
func Process(data []byte, mime string, maxDim int) (Result, error) {
	keep := Result{Data: data, MIME: mime}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		// 解不出来的东西不该进媒体库：MIME 是客户端说的，做不得数。
		return keep, fmt.Errorf("decode config: %w", err)
	}
	keep.Width, keep.Height = cfg.Width, cfg.Height
	if cfg.Width*cfg.Height > MaxPixels {
		return keep, ErrTooLarge
	}

	// 动图整个跳过。image.Decode 只会给出第一帧，转完就是一张静态图——
	// 那不是压缩，是把内容删了。
	if mime == "image/gif" && animated(data) {
		return keep, nil
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return keep, fmt.Errorf("decode: %w", err)
	}

	if maxDim > 0 {
		if scaled, ok := fit(img, maxDim); ok {
			img = scaled
			keep.Resized = true
			b := img.Bounds()
			keep.Width, keep.Height = b.Dx(), b.Dy()
		}
	}

	best, ok := encodeBest(img, mime)
	// 缩过的图必须换掉原文件：原图尺寸不对了，留着它等于没缩。
	// 这时即便 WebP 更大也用它——反正比原来那张大图小。
	if ok && (len(best) < len(data) || keep.Resized) {
		keep.Data, keep.MIME, keep.Converted = best, "image/webp", true
	} else if keep.Resized {
		// 缩了却编不出 WebP：宁可原样留着，也不要交出一张尺寸和记录
		// 对不上的图。把尺寸改回原值。
		keep.Width, keep.Height, keep.Resized = cfg.Width, cfg.Height, false
	}
	return keep, nil
}

// encodeBest 按来源挑编码方式，两种都试的时候取小的那个。
//
// 为什么不一律两种都试：JPEG 的无损 WebP 是在无损保存**JPEG 的压缩瑕疵**，
// 实测比原图大 390%，试它纯属浪费——那一次编码要一秒。
func encodeBest(img image.Image, srcMIME string) ([]byte, bool) {
	enc := func(o webp.Options) []byte {
		var buf bytes.Buffer
		if err := webp.Encode(&buf, img, o); err != nil {
			return nil
		}
		return buf.Bytes()
	}
	lossy := enc(webp.Options{Quality: Quality, Method: 4})
	if srcMIME == "image/jpeg" {
		return lossy, lossy != nil
	}
	// PNG / GIF / WebP：可能是照片，也可能是纯色截图，两种差距能到十倍，
	// 从格式上看不出来。编两次取小的，比猜一次可靠。
	lossless := enc(webp.Options{Lossless: true, Method: 4})
	switch {
	case lossy != nil && lossless != nil:
		if len(lossless) < len(lossy) {
			return lossless, true
		}
		return lossy, true
	case lossy != nil:
		return lossy, true
	case lossless != nil:
		return lossless, true
	}
	return nil, false
}

// fit 把长边压到 max 以内，等比。已经够小就原样返回。
func fit(src image.Image, max int) (image.Image, bool) {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= max && h <= max {
		return src, false
	}
	if w >= h {
		h, w = h*max/w, max
	} else {
		w, h = w*max/h, max
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	// CatmullRom 比 ApproxBiLinear 慢一截，但缩图是一次性的，而结果要
	// 在页面上待到文章被删。缩过头的图糊起来很显眼，这里不省这点时间。
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)
	return dst, true
}

// animated 判断 GIF 是不是多帧。
func animated(data []byte) bool {
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return true // 读不明白就当它是动图，不碰
	}
	return len(g.Image) > 1
}

// Dimensions 只读文件头拿像素尺寸，不解码。
//
// 放在这个包里而不是调用方自己 image.DecodeConfig：认得哪些格式取决于
// 谁 import 了对应的解码器，而那些 import 在这里。调用方靠"碰巧传递地
// import 到了"能跑，等哪天依赖变了就会变成"PNG 认得、WebP 不认得"。
func Dimensions(r io.Reader) (int, int, error) {
	cfg, _, err := image.DecodeConfig(r)
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}
