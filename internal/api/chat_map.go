package api

import (
	"context"

	"prism-2api/internal/adapter"
)

// toAdapterRequest flattens an OpenAI chat request into the vendor-neutral shape.
// systemPrompt 是调用方按热配置解析好的注入文本（空=不注入）。
// 附件（图片/文件）在这里解成字节，落地由适配器按账号做（见 adapter.File）。
func toAdapterRequest(ctx context.Context, req *ChatCompletionRequest, systemPrompt string) adapter.ChatRequest {
	out := adapter.ChatRequest{
		Model:           req.Model,
		Stream:          req.Stream,
		Temperature:     req.Temperature,
		ReasoningEffort: req.ReasoningEffort,
		SystemPrompt:    systemPrompt,
	}
	if req.MaxTokens != nil {
		out.MaxTokens = *req.MaxTokens
	} else if req.MaxCompletionTokens != nil {
		out.MaxTokens = *req.MaxCompletionTokens
	}
	for _, m := range req.Messages {
		msg := adapter.ChatMessage{
			Role:       m.Role,
			Content:    contentText(m.Content),
			ToolCallID: m.ToolCallID,
			Reasoning:  m.Reasoning,
		}
		for _, f := range extractFiles(ctx, m.Content) {
			msg.Files = append(msg.Files, adapter.File{Name: f.Name, Mime: f.Mime, Data: f.Data, Text: f.Text})
		}
		for _, tc := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, adapter.ToolCall{
				ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
			})
		}
		out.Messages = append(out.Messages, msg)
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, adapter.ToolDef{
			Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters,
		})
	}
	return out
}

// MapChat is the kernel entry used by OpenAI / Anthropic handlers.
func MapChat(ctx context.Context, req *ChatCompletionRequest, systemPrompt string) (*adapter.NativeRequest, error) {
	return adapter.MapChat(toAdapterRequest(ctx, req, systemPrompt))
}
