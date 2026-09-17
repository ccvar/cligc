package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client 是站点 REST API 的 HTTP 客户端。
//
// MCP 层刻意走 HTTP 而不是直接调 store：这样同一个二进制既能在服务器上
// 内嵌运行，也能作为 stdio 程序跑在用户笔记本上连远端站点，代码只有一份。
// 本机内嵌时是一次回环调用，开销可以忽略。
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// NewClient 构造客户端。base 形如 https://cligc.com（无尾斜杠）。
func NewClient(base, token string) *Client {
	return &Client{
		base:  strings.TrimRight(base, "/"),
		token: token,
		hc:    &http.Client{Timeout: 30 * time.Second},
	}
}

// APIError 携带服务端返回的稳定错误码，供工具层把它翻译成
// 模型能据以行动的提示。
type APIError struct {
	Status  int
	Code    string
	Message string
	Details string
}

func (e *APIError) Error() string {
	if e.Details != "" {
		return e.Message + " (" + e.Details + ")"
	}
	return e.Message
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.send(req, out)
}

func (c *Client) send(req *http.Request, out any) error {
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		e := &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(data))}
		var parsed struct {
			Error, Code, Details string
		}
		if json.Unmarshal(data, &parsed) == nil && parsed.Code != "" {
			e.Code, e.Message, e.Details = parsed.Code, parsed.Error, parsed.Details
		}
		return e
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// upload 以 multipart 形式上传一个文件。
func (c *Client) upload(ctx context.Context, filename, mime string, data []byte, out any) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{
		fmt.Sprintf(`form-data; name="file"; filename=%q`, filename)}
	h["Content-Type"] = []string{mime}
	part, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	mw.WriteField("mime", mime)
	if err := mw.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v1/media", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return c.send(req, out)
}
