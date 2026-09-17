package indexnow

import (
	"testing"
	"time"
)

// TestNilPingerIsSafe 守住"baseURL 不可用就彻底不启用"这条。
//
// New 在这种情况下返回 nil，接入点因此可以直接调用而不判空。这只有在所有
// 方法都能处理 nil 接收者时才成立——漏掉任何一个，站点就会在一个可选功能
// 没配置的情况下 panic。
func TestNilPingerIsSafe(t *testing.T) {
	for _, p := range []*Pinger{New(""), New("not a url"), New("https://")} {
		if p != nil {
			t.Fatal("baseURL 不可用时不该返回可用的 Pinger")
		}
		p.Ping("/p/foo")
		p.SetKey("abc")
		if p.Key() != "" || p.KeyFileURL() != "" || p.Enabled() {
			t.Error("nil Pinger 应当处处返回零值")
		}
	}
}

// TestKeyIsRuntimeSwitchable key 现在是后台里的设置，必须能随时开关，
// 而不是启动时定死。
func TestKeyIsRuntimeSwitchable(t *testing.T) {
	p := New("https://example.com/")
	if p == nil {
		t.Fatal("合法 baseURL 应当返回 Pinger")
	}
	if p.Enabled() {
		t.Error("还没设 key 就报告已启用")
	}
	p.SetKey("  DeadBeef  ")
	if got := p.Key(); got != "deadbeef" {
		t.Errorf("Key() = %q，应当去空白并转小写", got)
	}
	if !p.Enabled() {
		t.Error("设了 key 却报告未启用")
	}
	if got := p.KeyFileURL(); got != "https://example.com"+KeyPath {
		t.Errorf("KeyFileURL() = %q（baseURL 的尾斜杠没去掉？）", got)
	}
	p.SetKey("")
	if p.Enabled() {
		t.Error("清空 key 之后应当停止提交")
	}
}

// TestPingNeverBlocks 队列满了必须直接丢，不能反压到发布流程上。
func TestPingNeverBlocks(t *testing.T) {
	p := &Pinger{baseURL: "https://example.com", ch: make(chan string, 2)}
	p.SetKey("k")
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			p.Ping("/p/foo")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("队列满时 Ping 阻塞了 —— 发布流程会被第三方端点拖死")
	}
}
