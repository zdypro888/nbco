package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client 调 nbco worker 接口。
type Client struct {
	base  string
	token string
	http  *http.Client
}

func newClient(base, token string) *Client {
	return &Client{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Task 从 nbco 领到的任务。
type Task struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Goal        string `json:"goal"`
	Description string `json:"description"`
	Acceptance  string `json:"acceptance"`
}

// Next 认领下一个任务；无任务返回 (nil, nil, nil)。knowledge 是相关历史经验。
func (c *Client) Next(ctx context.Context) (*Task, []string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/worker/next", nil)
	c.auth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, c.errStatus(resp)
	}
	var body struct {
		Task      Task     `json:"task"`
		Knowledge []string `json:"knowledge"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, nil, err
	}
	return &body.Task, body.Knowledge, nil
}

// Progress 回传一段执行进度。
func (c *Client) Progress(ctx context.Context, taskID int64, content string) error {
	return c.post(ctx, "/api/worker/progress", map[string]any{"task_id": taskID, "content": content})
}

// Submit 提交完成，进入验收流；lessons 非空则回流知识库。
func (c *Client) Submit(ctx context.Context, taskID int64, summary, lessons string) error {
	return c.post(ctx, "/api/worker/submit", map[string]any{
		"task_id": taskID, "summary": summary, "lessons": lessons,
	})
}

func (c *Client) post(ctx context.Context, path string, payload any) error {
	buf, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c.errStatus(resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *Client) auth(req *http.Request) { req.Header.Set("Authorization", "Bearer "+c.token) }

func (c *Client) errStatus(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
}
