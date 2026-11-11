package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ============================================================================
// 大模型客户端：OpenAI 兼容协议（/chat/completions）
//
// 兼容范围（只要对方提供 OpenAI 风格接口即可）：
//   DeepSeek      base_url: https://api.deepseek.com          model: deepseek-flash
//   OpenAI        base_url: https://api.openai.com/v1         model: gpt-4o-mini
//   Qwen/DashScope base_url: https://dashscope.aliyuncs.com/compatible-mode/v1
//   Kimi/Moonshot base_url: https://api.moonshot.cn/v1
//   Zhipu         base_url: https://open.bigmodel.cn/api/paas/v4
//   Ollama        base_url: http://127.0.0.1:11434/v1        model: qwen2.5
//   vLLM/LM Studio 同理，指向本地 /v1 即可
// ============================================================================

// ModelConfig 对应 JSON 里的 "model" 段。
type ModelConfig struct {
	Name         string            `json:"name"`          // 展示名，例如 "DeepSeek"
	BaseURL      string            `json:"base_url"`      // 接口地址，可写 .../v1 或省略由程序补全
	APIKey       string            `json:"api_key"`       // 密钥，支持 ${ENV}
	Model        string            `json:"model"`         // 模型名，例如 deepseek-flash
	TimeoutSec   int               `json:"timeout_sec"`   // 单次请求超时，默认 120
	Temperature  float64           `json:"temperature"`   // 默认 0.2，运维场景偏确定性
	MaxTokens    int               `json:"max_tokens"`    // 默认 2048
	SystemPrompt string            `json:"system_prompt"` // 可选：覆盖内置系统提示
	ExtraHeaders map[string]string `json:"extra_headers"` // 可选：额外请求头
}

// chatMessage 是对话消息，兼容 OpenAI 的 tool_calls 结构。
type chatMessage struct {
	Role       string     `json:"role"` // system | user | assistant | tool
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // function
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串
}

// toolDef 是提供给模型的工具定义。
type toolDef struct {
	Type     string      `json:"type"` // function
	Function toolFuncDef `json:"function"`
}

type toolFuncDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// chatRequest 是 /chat/completions 请求体。
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Tools       []toolDef     `json:"tools,omitempty"`
	ToolChoice  string        `json:"tool_choice,omitempty"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Stream      bool          `json:"stream"`
}

// chatResponse 是 /chat/completions 响应体。
type chatResponse struct {
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// LLMClient 是大模型的 HTTP 客户端。
type LLMClient struct {
	cfg  ModelConfig
	http *http.Client
	url  string
}

// NewLLMClient 构造客户端，并做配置校验。
func NewLLMClient(cfg ModelConfig) (*LLMClient, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("model.base_url 未配置")
	}
	if cfg.Model == "" {
		return nil, errors.New("model.model 未配置（例如 deepseek-flash / gpt-4o-mini / qwen2.5）")
	}
	if ref := undefinedEnvRef(cfg.APIKey); ref != "" {
		return nil, fmt.Errorf("model.api_key 引用的环境变量 %s 未设置", ref)
	}
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &LLMClient{
		cfg:  cfg,
		http: &http.Client{Timeout: timeout},
		url:  chatCompletionsURL(cfg.BaseURL),
	}, nil
}

// chatCompletionsURL 把 base_url 规整成完整的 chat/completions 地址。
// 支持三种写法：
//
//	https://api.deepseek.com            -> https://api.deepseek.com/chat/completions
//	https://api.openai.com/v1           -> .../v1/chat/completions
//	https://xxx/v1/chat/completions     -> 原样使用
func chatCompletionsURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	// 已带版本段（/v1、/v4 等）直接拼
	last := base[strings.LastIndex(base, "/")+1:]
	if isVersionSegment(last) {
		return base + "/chat/completions"
	}
	// DeepSeek 与部分厂商根路径直接可用；OpenAI 等需要 /v1。
	// 统一策略：没写版本段就补 /v1，但 DeepSeek 官方根域名除外。
	if strings.Contains(base, "api.deepseek.com") {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

func isVersionSegment(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for _, r := range s[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Chat 发一次请求，返回助手消息（可能含 tool_calls）。
func (c *LLMClient) Chat(ctx context.Context, messages []chatMessage, tools []toolDef) (chatMessage, error) {
	temp := c.cfg.Temperature
	req := chatRequest{
		Model: c.cfg.Model, Messages: messages, Tools: tools,
		Temperature: temp, MaxTokens: c.cfg.MaxTokens,
	}
	if len(tools) > 0 {
		req.ToolChoice = "auto"
	}
	body, err := json.Marshal(req)
	if err != nil {
		return chatMessage{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return chatMessage{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	for k, v := range c.cfg.ExtraHeaders {
		httpReq.Header.Set(k, v)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return chatMessage{}, fmt.Errorf("请求模型接口失败: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return chatMessage{}, fmt.Errorf("读取模型响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := friendlyAPIError(data)
		switch resp.StatusCode {
		case 401, 403:
			msg += "\n提示: API Key 无效、过期或无权限，检查 model.api_key（或环境变量）是否正确"
		case 404:
			msg += "\n提示: 接口路径不对，检查 model.base_url（多数厂商需要以 /v1 结尾）与 model.model 名称"
		case 429:
			msg += "\n提示: 触发限流或额度不足，稍后再试"
		}
		return chatMessage{}, fmt.Errorf("模型接口返回 HTTP %d: %s", resp.StatusCode, msg)
	}

	var cr chatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return chatMessage{}, fmt.Errorf("解析模型响应失败（接口可能不是 OpenAI 兼容格式）: %v", err)
	}
	if cr.Error != nil && cr.Error.Message != "" {
		return chatMessage{}, fmt.Errorf("模型返回错误: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return chatMessage{}, errors.New("模型没有返回任何回复")
	}
	msg := cr.Choices[0].Message
	if msg.Role == "" {
		msg.Role = "assistant"
	}
	return msg, nil
}

// friendlyAPIError 把常见错误翻译成可操作的提示。
func friendlyAPIError(data []byte) string {
	var cr chatResponse
	if json.Unmarshal(data, &cr) == nil && cr.Error != nil && cr.Error.Message != "" {
		return cr.Error.Message
	}
	s := strings.TrimSpace(string(data))
	if len(s) > 400 {
		s = s[:400] + "..."
	}
	lower := strings.ToLower(s)
	switch {
	case strings.Contains(lower, "invalid api key") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "401"):
		s += "\n提示: API Key 无效或已过期，检查 model.api_key（或环境变量）"
	case strings.Contains(lower, "404") || strings.Contains(lower, "not found"):
		s += "\n提示: 接口路径不对，检查 model.base_url（多数厂商需要以 /v1 结尾）"
	case strings.Contains(lower, "rate limit") || strings.Contains(lower, "429"):
		s += "\n提示: 触发限流，稍后重试或降低并发"
	}
	return s
}

// PingModel 启动自检：发一条极短消息确认 Key、地址、模型名都对。
func (c *LLMClient) PingModel(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := c.Chat(ctx, []chatMessage{{Role: "user", Content: "回复一个词：ok"}}, nil)
	return err
}

// ListModels 调用 /models 查询该接口实际可用的模型名。
// 大多数 OpenAI 兼容接口都支持；不支持的返回错误，由调用方回退到预设列表。
func ListModels(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	base = strings.TrimSuffix(base, "/chat/completions")
	if i := strings.LastIndex(base, "/chat/completions"); i > 0 {
		base = base[:i]
	}
	modelsURL := base + "/models"
	if !isVersionSegment(base[strings.LastIndex(base, "/")+1:]) && !strings.Contains(base, "api.deepseek.com") {
		modelsURL = base + "/v1/models"
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range out.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("接口没有返回任何模型")
	}
	return ids, nil
}
