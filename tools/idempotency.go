package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
)

const (
	invocationReceiptTimeout = 10 * time.Second
	invocationResultTTL      = 24 * time.Hour
)

// withInvocationIdempotency extends Eino's in-memory exact-call guard across
// process restarts and reclaimed automation runs. Direct callers without a
// runtime-owned invocation identity keep the existing behavior; their transport
// boundary must provide its own idempotency contract.
func withInvocationIdempotency(s *store.Store, userID int64, t ai.Tool) ai.Tool {
	if s == nil || userID <= 0 || (t.Effect != ai.ToolEffectWrite && t.Effect != ai.ToolEffectExecute) || t.Handler == nil {
		return t
	}
	inner := t.Handler
	recoverResult := t.RecoverResult
	resultPersisted := t.ResultPersisted
	name := t.Name
	completion := t.Completion
	t.Handler = func(ctx context.Context, args json.RawMessage) (string, error) {
		invocation := strings.TrimSpace(ai.ToolInvocationKey(ctx))
		if invocation == "" {
			return inner(ctx, args)
		}
		key := toolInvocationReceiptKey(userID, name, invocation)
		receipt, created, err := s.BeginExternalAction(ctx, key, "tool:"+name, canonicalArgsHash(args))
		if err != nil {
			if receipt != nil {
				return uncertainInvocationReplayResult(), nil
			}
			return "", fmt.Errorf("登记工具调用幂等边界: %w", err)
		}
		if !created {
			if receipt.Status != store.ExternalActionCompleted && recoverResult != nil {
				out, recovered, recoverErr := recoverResult(ctx, args)
				if recoverErr != nil {
					return "", fmt.Errorf("恢复工具调用结果: %w", recoverErr)
				}
				if recovered {
					out = truncateToolOutput(out)
					ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), invocationReceiptTimeout)
					defer cancel()
					if err := s.CompleteRecoverableExternalActionWithResult(ackCtx, key, out, time.Now().UTC().Add(invocationResultTTL)); err != nil {
						return "", fmt.Errorf("保存恢复的工具调用结果: %w", err)
					}
					runResultPersisted(resultPersisted, ackCtx, args, out, name)
					return out, nil
				}
			}
			return settledInvocationReplayResult(receipt, completion), nil
		}

		out, callErr := inner(ctx, args)
		ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), invocationReceiptTimeout)
		defer cancel()
		if callErr != nil {
			cause := textfmt.RedactSecrets(callErr.Error())
			if markErr := s.FailExternalAction(ackCtx, key, cause); markErr != nil {
				return out, fmt.Errorf("工具调用失败: %w；失败状态保存失败: %v", callErr, markErr)
			}
			return out, callErr
		}
		persistedResult := truncateToolOutput(out)
		if err := s.CompleteExternalActionWithResult(ackCtx, key, persistedResult, time.Now().UTC().Add(invocationResultTTL)); err != nil {
			return "", fmt.Errorf("工具已返回但幂等状态保存失败: %w", err)
		}
		runResultPersisted(resultPersisted, ackCtx, args, persistedResult, name)
		return out, nil
	}
	return t
}

func runResultPersisted(finalize func(context.Context, json.RawMessage, string) error, ctx context.Context, args json.RawMessage, result, name string) {
	if finalize == nil {
		return
	}
	if err := finalize(ctx, args, result); err != nil {
		slog.Warn("工具恢复数据清理失败", "tool", name, "err", textfmt.RedactSecrets(err.Error()))
	}
}

func toolInvocationRequestKey(ctx context.Context, userID int64, name string) string {
	invocation := strings.TrimSpace(ai.ToolInvocationKey(ctx))
	if invocation == "" {
		return ""
	}
	return toolInvocationReceiptKey(userID, name, invocation)
}

func recoverableResultTool(t ai.Tool, recover func(context.Context, json.RawMessage) (string, bool, error)) ai.Tool {
	t.RecoverResult = recover
	return t
}

func recoverableResultToolWithFinalize(
	t ai.Tool,
	recover func(context.Context, json.RawMessage) (string, bool, error),
	finalize func(context.Context, json.RawMessage, string) error,
) ai.Tool {
	t.RecoverResult = recover
	t.ResultPersisted = finalize
	return t
}

func toolInvocationReceiptKey(userID int64, name, invocation string) string {
	sum := sha256.Sum256([]byte(strconv.FormatInt(userID, 10) + "\x00" + strings.TrimSpace(name) + "\x00" + strings.TrimSpace(invocation)))
	return "agent-tool:" + hex.EncodeToString(sum[:])
}

func settledInvocationReplayResult(receipt *store.ExternalActionReceipt, completion ai.ToolCompletion) string {
	if receipt != nil && receipt.Status == store.ExternalActionCompleted {
		if receipt.ResultUntil != nil && time.Now().UTC().Before(*receipt.ResultUntil) {
			return receipt.ResultText
		}
		message := "该逻辑工具调用此前已经完成，本次没有再次执行，但原始工具结果已超过短期恢复期限。请使用只读工具核实当前状态；不要把本条提示当作本轮执行成功的证据。"
		if completion == ai.ToolCompletionAsynchronous {
			message += " 异步工作可能仍在运行或已结束，请查询对应任务或执行记录。"
		}
		return encodeToolResult(ToolResultEnvelope{
			Status:    "rejected",
			ErrorType: "execution_result_expired",
			Message:   message,
		})
	}
	return uncertainInvocationReplayResult()
}

func uncertainInvocationReplayResult() string {
	return encodeToolResult(ToolResultEnvelope{
		Status:    "rejected",
		ErrorType: "execution_state_unknown",
		Message:   "该逻辑工具调用此前已越过执行边界，但最终状态无法由当前请求证明。系统为避免重复副作用没有重放；请使用只读工具核实当前状态。",
	})
}
