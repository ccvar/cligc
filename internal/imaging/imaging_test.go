package imaging

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"math/rand"
	"testing"
)

// photo 造一张"照片样"的图：平滑渐变加噪点，有损编码的主场。
func photo(w, h int) image.Image {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewSource(1))
	for y := range h {
		for x := range w {
			n := func(v float64) uint8 {
				v += rng.NormFloat64() * 7
				return uint8(math.Max(0, math.Min(255, v)))
			}
			m.Set(x, y, color.RGBA{
				n(128 + 110*math.Sin(float64(x)/19)),
				n(128 + 110*math.Sin(float64(y)/13+1)),
				n(128 + 110*math.Sin(float64(x+y)/24+2)), 255})
		}
	}
	return m
}

// flat 造一张大片纯色加硬边的图：有损编码最不擅长的那种。
func flat(w, h int) image.Image {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(m, m.Bounds(), &image.Uniform{color.RGBA{250, 250, 249, 255}}, image.Point{}, draw.Src)
	draw.Draw(m, image.Rect(w/8, h/8, w/2, h/2),
		&image.Uniform{color.RGBA{30, 40, 200, 255}}, image.Point{}, draw.Src)
	return m
}

func asJPEG(t *testing.T, m image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, m, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// jpegQ 按指定质量编码，用来造"已经压得很狠"的图。
func jpegQ(t *testing.T, m image.Image, q int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, m, &jpeg.Options{Quality: q}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func asPNG(t *testing.T, m image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestPhotoShrinks 照片转 WebP 要明显变小。这是做这整件事的理由，
// 省不下来就不值得引入一个 4.6 MB 的编码器。
func TestPhotoShrinks(t *testing.T) {
	src := asJPEG(t, photo(800, 600))
	got, err := Process(src, "image/jpeg", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Converted || got.MIME != "image/webp" {
		t.Fatalf("没转成 WebP：mime=%s converted=%v", got.MIME, got.Converted)
	}
	if len(got.Data) >= len(src) {
		t.Errorf("转完 %d 字节，原图 %d 字节，没变小", len(got.Data), len(src))
	}
	if got.Width != 800 || got.Height != 600 {
		t.Errorf("尺寸 %dx%d，不该动", got.Width, got.Height)
	}
}

// TestNeverGrows 纯色图的有损 WebP 实测比 PNG 大 26%。
// 无论走哪条分支，出来的文件都不能比进去的大。
func TestNeverGrows(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  []byte
		mime string
	}{
		{"纯色 PNG", asPNG(t, flat(500, 380)), "image/png"},
		{"照片 PNG", asPNG(t, photo(320, 240)), "image/png"},
		{"照片 JPEG", asJPEG(t, photo(320, 240)), "image/jpeg"},
		// 已经压得很狠的 JPEG：WebP q80 反而更大，必须原样留着。
		// 这一条是这个测试真正的靶子——上面三条都是会变小的皆大欢喜情形，
		// 只有它逼着"没变小就不换"那条分支跑起来。
		{"低质量 JPEG", jpegQ(t, photo(400, 300), 25), "image/jpeg"},
	} {
		got, err := Process(tc.src, tc.mime, 0)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		// 明确钉住那条分支真的被走到了。不钉的话，哪天图改小一点、
		// WebP 又变得划算，这个测试会继续绿，而回退逻辑其实没人验过。
		if tc.name == "低质量 JPEG" {
			if got.Converted {
				t.Errorf("%s: 转成了 %d 字节的 WebP（原图 %d），"+
					"这一条本该走回退分支——换个更低的质量再试",
					tc.name, len(got.Data), len(tc.src))
			}
			if got.MIME != "image/jpeg" {
				t.Errorf("%s: mime 变成了 %s，没转换就该保持原样", tc.name, got.MIME)
			}
		}
		if len(got.Data) > len(tc.src) {
			t.Errorf("%s: 出来 %d 字节，进去 %d 字节——上传之后变大了",
				tc.name, len(got.Data), len(tc.src))
		}
		// 没转换时必须原样交还，不能是"重新编码过的原格式"。
		if !got.Converted && !bytes.Equal(got.Data, tc.src) {
			t.Errorf("%s: 没标 Converted 却换了字节", tc.name)
		}
	}
}

// TestResizeCapsLongEdge 超过上限的图按长边等比缩。
func TestResizeCapsLongEdge(t *testing.T) {
	src := asJPEG(t, photo(1200, 400))
	got, err := Process(src, "image/jpeg", 600)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Resized {
		t.Fatal("超过上限却没缩")
	}
	if got.Width != 600 || got.Height != 200 {
		t.Errorf("缩成 %dx%d，想要 600x200（等比）", got.Width, got.Height)
	}
	// 缩完的尺寸必须和真实像素一致，不能只改记录。
	cfg, _, err := image.DecodeConfig(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != got.Width || cfg.Height != got.Height {
		t.Errorf("记录 %dx%d，文件里是 %dx%d", got.Width, got.Height, cfg.Width, cfg.Height)
	}
}

// TestSmallImageNotResized 没超上限的不动。等比放大只会变糊变大。
func TestSmallImageNotResized(t *testing.T) {
	got, err := Process(asJPEG(t, photo(300, 200)), "image/jpeg", 2560)
	if err != nil {
		t.Fatal(err)
	}
	if got.Resized || got.Width != 300 || got.Height != 200 {
		t.Errorf("小图被动过：%dx%d resized=%v", got.Width, got.Height, got.Resized)
	}
}

// TestAnimatedGIFUntouched 动图整个跳过。
//
// image.Decode 只给第一帧，转完就是一张静态图——那不是压缩，是把内容删了。
func TestAnimatedGIFUntouched(t *testing.T) {
	pal := color.Palette{color.Black, color.White}
	var frames []*image.Paletted
	for i := range 3 {
		f := image.NewPaletted(image.Rect(0, 0, 40, 40), pal)
		f.SetColorIndex(i, i, 1)
		frames = append(frames, f)
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, &gif.GIF{Image: frames, Delay: []int{10, 10, 10}}); err != nil {
		t.Fatal(err)
	}
	src := b.Bytes()

	got, err := Process(src, "image/gif", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Converted {
		t.Error("动图被转成了 WebP，只剩第一帧")
	}
	if !bytes.Equal(got.Data, src) {
		t.Error("动图的字节被动过了")
	}
	if g, err := gif.DecodeAll(bytes.NewReader(got.Data)); err != nil || len(g.Image) != 3 {
		t.Errorf("出来的不再是 3 帧动图：%v", err)
	}
}

// TestStaticGIFConverts 单帧 GIF 没有这个顾虑，照转。
func TestStaticGIFConverts(t *testing.T) {
	var b bytes.Buffer
	if err := gif.Encode(&b, flat(400, 300), nil); err != nil {
		t.Fatal(err)
	}
	got, err := Process(b.Bytes(), "image/gif", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Converted {
		t.Errorf("单帧 GIF 没转（%d → %d 字节）", b.Len(), len(got.Data))
	}
}

// TestRejectsDecompressionBomb 声明了巨大尺寸的图在**解码前**就要被拦下。
//
// 一个几十 KB 的 PNG 可以声明 30000×30000，解出来要几个 GB。等 image.Decode
// 返回再检查就晚了——内存已经分配出去了。
func TestRejectsDecompressionBomb(t *testing.T) {
	// 手搓一个只有 IHDR 的 PNG，头里声明 20000×20000（4 亿像素）。
	//
	// 不用 png.Encode 真造一张：那要先在内存里摆出 4 亿个像素再压一遍，
	// 光造测试数据就是 27 秒（-race 下），而这个测试要验的恰恰是
	// "在解码之前就拦下"——真造出来反而说明拦不住的那条路先跑完了。
	// 炸弹的实质就是"头里写得大、文件本身很小"，手搓的这个正是如此。
	ihdr := binary.BigEndian.AppendUint32(nil, 20000)
	ihdr = binary.BigEndian.AppendUint32(ihdr, 20000)
	ihdr = append(ihdr, 8, 0, 0, 0, 0) // 位深 8、灰度、默认压缩/滤波/隔行
	chunk := func(kind string, data []byte) []byte {
		out := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
		body := append([]byte(kind), data...)
		out = append(out, body...)
		return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(body))
	}
	bomb := append([]byte("\x89PNG\r\n\x1a\n"), chunk("IHDR", ihdr)...)

	if _, err := Process(bomb, "image/png", 0); !errors.Is(err, ErrTooLarge) {
		t.Errorf("4 亿像素的图 = %v，想要 ErrTooLarge", err)
	}
	if len(bomb) > 100 {
		t.Errorf("测试数据 %d 字节——它该是个小文件，否则就不叫炸弹了", len(bomb))
	}
}

// TestRejectsNonImage MIME 是客户端说的，做不得数。
func TestRejectsNonImage(t *testing.T) {
	if _, err := Process([]byte("<?php system($_GET['c']); ?>"), "image/png", 0); err == nil {
		t.Error("解不出来的东西不该当成图片收下")
	}
}

// TestWebPInputStaysReadable 已经是 WebP 的上传也要能读出尺寸。
func TestWebPInputStaysReadable(t *testing.T) {
	first, err := Process(asJPEG(t, photo(300, 220)), "image/jpeg", 0)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Process(first.Data, "image/webp", 0)
	if err != nil {
		t.Fatal(err)
	}
	if again.Width != 300 || again.Height != 220 {
		t.Errorf("重新读 WebP 得到 %dx%d，想要 300x220", again.Width, again.Height)
	}
	if len(again.Data) > len(first.Data) {
		t.Error("二次处理让 WebP 变大了")
	}
}
