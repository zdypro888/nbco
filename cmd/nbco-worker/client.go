package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zdypro888/nbco/taskflow"
)

// fileTransferTimeout 单次文件收发的墙钟上限（兜底，防连接挂死；大文件不该被
// 控制面的 30s 掐断）。
const fileTransferTimeout = 30 * time.Minute

// Client 调 nbco worker 接口。
type Client struct {
	base  string
	token string
	http  *http.Client // 控制面小 JSON 调用：30s 整体超时
	files *http.Client // 文件收发：无整体超时，靠 per-call ctx 限时
}

func newClient(base, token string) *Client {
	return &Client{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
		files: &http.Client{}, // 大文件收发不受 30s 墙钟限制
	}
}

// bindCodePrefix 一次性绑定码前缀（与服务端 store.WorkerBindCodePrefix 约定一致；
// 不 import store，避免把数据库依赖链进 worker 二进制）。
const bindCodePrefix = "wbc_"

// isBindCode 判断凭据是绑定码还是 access token。
func isBindCode(cred string) bool { return strings.HasPrefix(cred, bindCodePrefix) }

// BindResult 绑定码兑换结果。
type BindResult struct {
	Token      string `json:"token"`
	WorkerID   int64  `json:"worker_id"`
	WorkerName string `json:"worker_name"`
}

type CapabilityReport struct {
	Engine       string         `json:"engine"`
	CLIName      string         `json:"cli_name"`
	CLIVersion   string         `json:"cli_version"`
	OS           string         `json:"os"`
	Arch         string         `json:"arch"`
	Hostname     string         `json:"hostname"`
	Workdir      string         `json:"workdir"`
	Capabilities []string       `json:"capabilities"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// RedeemBindCode 用一次性绑定码兑换 Worker Access Token（无需已有 token）。
func (c *Client) RedeemBindCode(ctx context.Context, code string) (*BindResult, error) {
	buf, _ := json.Marshal(map[string]string{"code": code})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/worker/bind", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.errStatus(resp)
	}
	var res BindResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Identity 是当前 token 对应的 nbco 用户身份。
type Identity struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	IsSuperadmin bool   `json:"is_superadmin"`
	IsWorker     bool   `json:"is_worker"`
	OwnerID      *int64 `json:"owner_id"`
}

// Me 校验 token 并返回当前身份。
func (c *Client) Me(ctx context.Context) (*Identity, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/me", nil)
	c.auth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.errStatus(resp)
	}
	var ident Identity
	if err := json.NewDecoder(resp.Body).Decode(&ident); err != nil {
		return nil, err
	}
	return &ident, nil
}

func (c *Client) RegisterCapabilities(ctx context.Context, report CapabilityReport) error {
	return c.post(ctx, "/api/worker/capabilities", report)
}

// Task 从 nbco 领到的任务。
type Task struct {
	ID               int64        `json:"id"`
	ClaimID          string       `json:"claim_id"`
	Title            string       `json:"title"`
	Goal             string       `json:"goal"`
	Description      string       `json:"description"`
	Acceptance       string       `json:"acceptance"`
	CompletionPolicy string       `json:"completion_policy"`
	Command          string       `json:"command"`
	CommandPTY       bool         `json:"command_pty"`
	Attachments      []Attachment `json:"attachments"`
	Session          SessionInfo  `json:"session"`
}

// SessionInfo is the server-owned topic context this task belongs to.
type SessionInfo struct {
	ID               int64  `json:"id"`
	Engine           string `json:"engine"`
	ScopeType        string `json:"scope_type"`
	ScopeKey         string `json:"scope_key"`
	Title            string `json:"title"`
	Workdir          string `json:"workdir,omitempty"`
	EngineSessionRef string `json:"engine_session_ref,omitempty"`
	Summary          string `json:"summary,omitempty"`
}

// Attachment 是服务端随任务下发的文件附件。
type Attachment struct {
	ID           int64  `json:"id"`
	OriginalName string `json:"original_name"`
	Kind         string `json:"kind,omitempty"`
	Caption      string `json:"caption,omitempty"`
	MIMEType     string `json:"mime_type"`
	SizeBytes    int64  `json:"size_bytes"`
	SHA256       string `json:"sha256"`
	DownloadURL  string `json:"download_url"`
	LocalPath    string `json:"-"`
}

// Next 认领下一个任务；无任务返回全 nil。knowledge 是相关历史经验，
// history 是该任务已有的过程记录（返工时含验收打回理由）。
func (c *Client) Next(ctx context.Context, engine string) (*Task, []string, []string, error) {
	u := c.base + "/api/worker/next"
	if strings.TrimSpace(engine) != "" {
		u += "?engine=" + url.QueryEscape(strings.TrimSpace(engine))
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	c.auth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil, nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, nil, c.errStatus(resp)
	}
	var body struct {
		Task      Task     `json:"task"`
		Knowledge []string `json:"knowledge"`
		History   []string `json:"history"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, nil, nil, err
	}
	return &body.Task, body.Knowledge, body.History, nil
}

// llmCallTimeout 单次模型调用的墙钟上限（走 files client，不受 30s 控制面超时限制）。
const llmCallTimeout = 6 * time.Minute

// LLM 经中枢管道调一次模型（OpenAI 兼容 function calling），返回首个候选消息。
// model 由中枢钉死，worker 只发 messages + tools。
func (c *Client) LLM(ctx context.Context, messages []chatMessage, tools []map[string]any) (chatMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, llmCallTimeout)
	defer cancel()
	buf, err := json.Marshal(map[string]any{"messages": messages, "tools": tools})
	if err != nil {
		return chatMessage{}, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/worker/llm", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	resp, err := c.files.Do(req)
	if err != nil {
		return chatMessage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return chatMessage{}, c.errStatus(resp)
	}
	var body struct {
		Choices []struct {
			Message chatMessage `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return chatMessage{}, fmt.Errorf("解析模型响应失败: %w", err)
	}
	if body.Error != nil {
		return chatMessage{}, fmt.Errorf("上游模型错误: %s", body.Error.Message)
	}
	if len(body.Choices) == 0 {
		return chatMessage{}, fmt.Errorf("上游模型未返回内容")
	}
	return body.Choices[0].Message, nil
}

// Progress 回传一段执行进度。
func (c *Client) Progress(ctx context.Context, taskID int64, claimID, content string) error {
	return c.post(ctx, "/api/worker/progress", map[string]any{"task_id": taskID, "claim_id": claimID, "content": content})
}

func (c *Client) UpdateSession(ctx context.Context, taskID int64, claimID string, session SessionInfo, workdir string) error {
	return c.post(ctx, "/api/worker/session", map[string]any{
		"task_id": taskID, "claim_id": claimID,
		"worker_session_id": session.ID, "session_summary": session.Summary,
		"engine_session_ref": session.EngineSessionRef, "workdir": workdir,
	})
}

// RequestInput asks the task assigner for missing information without marking
// the task as done. The server pauses the task and releases the claim until the
// assigner updates or reassigns it, so it cannot be reclaimed as a stale run.
func (c *Client) RequestInput(ctx context.Context, taskID int64, claimID, content string, session SessionInfo, workdir string) error {
	return c.post(ctx, "/api/worker/request-input", map[string]any{
		"task_id": taskID, "claim_id": claimID, "content": content,
		"worker_session_id": session.ID, "session_summary": "等待补充：" + strings.TrimSpace(content),
		"engine_session_ref": session.EngineSessionRef, "workdir": workdir,
	})
}

// Fail releases a task claim through the server's durable retry policy. Session
// metadata is sent as well so a later retry can resume the same native CLI
// conversation and workspace instead of starting over.
func (c *Client) Fail(ctx context.Context, taskID int64, claimID, cause string, session SessionInfo, workdir string) error {
	return c.post(ctx, "/api/worker/fail", map[string]any{
		"task_id": taskID, "claim_id": claimID, "error": cause,
		"worker_session_id": session.ID, "session_summary": "最近执行失败：" + strings.TrimSpace(cause),
		"engine_session_ref": session.EngineSessionRef, "workdir": workdir,
	})
}

type SubmissionResult struct {
	Outcome  taskflow.ExecutionOutcome
	ExitCode *int
}

// Submit reports an execution-neutral outcome. ExitCode is optional evidence;
// task completion behavior is owned by the server-side completion policy.
func (c *Client) Submit(ctx context.Context, taskID int64, claimID, summary, lessons string, session SessionInfo, workdir string, result SubmissionResult) error {
	if !result.Outcome.Valid() {
		return fmt.Errorf("invalid submission outcome %q", result.Outcome)
	}
	payload := map[string]any{
		"task_id": taskID, "claim_id": claimID, "summary": summary, "lessons": lessons,
		"worker_session_id": session.ID, "session_summary": sessionSummary(summary, lessons),
		"engine_session_ref": session.EngineSessionRef, "workdir": workdir,
		"outcome": result.Outcome,
	}
	if result.ExitCode != nil {
		payload["exit_code"] = *result.ExitCode
	}
	return c.post(ctx, "/api/worker/submit", payload)
}

func sessionSummary(summary, lessons string) string {
	summary = strings.TrimSpace(summary)
	lessons = strings.TrimSpace(lessons)
	switch {
	case summary != "" && lessons != "":
		return clipHead("最近完成："+summary+"\n可复用经验："+lessons, 1200)
	case summary != "":
		return clipHead("最近完成："+summary, 1200)
	case lessons != "":
		return clipHead("可复用经验："+lessons, 1200)
	default:
		return ""
	}
}

// DownloadFile 下载 worker 被授权的文件到 dst。
func (c *Client) DownloadFile(ctx context.Context, urlPath, dst string) error {
	ctx, cancel := context.WithTimeout(ctx, fileTransferTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+urlPath, nil)
	c.auth(req)
	resp, err := c.files.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c.errStatus(resp)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(dst), ".nbco-download-*")
	if err != nil {
		return err
	}
	tmp := out.Name()
	defer os.Remove(tmp)
	_, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	return replaceFile(tmp, dst)
}

// UploadArtifact 上传一个已安全打开的产物文件（r 通常是校验过的 *os.File）。
// task_id/claim_id 走 query（而非 multipart 字段），服务端可在解析文件体之前
// 就校验 claim、拒绝时不把大文件 spool 到临时盘。
func (c *Client) UploadArtifact(ctx context.Context, taskID int64, claimID, name string, r io.Reader) error {
	ctx, cancel := context.WithTimeout(ctx, fileTransferTimeout)
	defer cancel()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	writeErr := make(chan error, 1)
	go func() {
		part, err := mw.CreateFormFile("file", name)
		if err == nil {
			_, err = io.Copy(part, r)
		}
		if cerr := mw.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = pw.CloseWithError(err)
		} else {
			err = pw.Close()
		}
		writeErr <- err
	}()
	q := url.Values{"task_id": {fmt.Sprint(taskID)}, "claim_id": {claimID}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/worker/artifacts?"+q.Encode(), pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	c.auth(req)
	resp, err := c.files.Do(req)
	if err != nil {
		_ = pr.CloseWithError(err)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := c.errStatus(resp)
		cancel()
		_ = pr.CloseWithError(err)
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if err := <-writeErr; err != nil {
		return err
	}
	return nil
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
