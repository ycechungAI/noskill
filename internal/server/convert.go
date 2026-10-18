package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"noskill/internal/backends"
)

// chatMessage is a message in the form the rest of the server uses, whatever the client spoke.
type chatMessage struct {
	Role    string
	Content string
	Images  [][]byte
}

// chatRequest is a chat request after parsing OpenAI or Ollama JSON.
type chatRequest struct {
	Model        string
	Messages     []chatMessage
	Stream       bool
	Temperature  *float64
	TopP         *float64
	MaxTokens    int
	Seed         *int
	Stop         []string
	Think        *bool
	Format       json.RawMessage
	IncludeUsage bool
	HasTools     bool // the client sent its own tools
	Raw          bool // Ollama generate with raw: true
}

type badRequest string

func (b badRequest) Error() string { return string(b) }

func bad(f string, a ...any) error { return badRequest(fmt.Sprintf(f, a...)) }

// maxImageBytes bounds one decoded image; larger ones are refused rather than held in memory.
const maxImageBytes = 20 << 20

// ---- OpenAI ----

type oaMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []oaToolCall    `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func parseOpenAI(body []byte) (chatRequest, error) {
	var in struct {
		Model         string      `json:"model"`
		Messages      []oaMessage `json:"messages"`
		Stream        bool        `json:"stream"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		Temperature         *float64          `json:"temperature"`
		TopP                *float64          `json:"top_p"`
		MaxTokens           int               `json:"max_tokens"`
		MaxCompletionTokens int               `json:"max_completion_tokens"`
		Seed                *int              `json:"seed"`
		Stop                json.RawMessage   `json:"stop"`
		ReasoningEffort     string            `json:"reasoning_effort"`
		ResponseFormat      json.RawMessage   `json:"response_format"`
		Tools               []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return chatRequest{}, bad("the request body isn't valid JSON: %v", err)
	}
	if in.Model == "" {
		return chatRequest{}, bad(`"model" is required`)
	}
	if len(in.Messages) == 0 {
		return chatRequest{}, bad(`"messages" is required`)
	}
	r := chatRequest{Model: in.Model, Stream: in.Stream, Temperature: in.Temperature, TopP: in.TopP, Seed: in.Seed,
		MaxTokens: in.MaxTokens, HasTools: len(in.Tools) > 0, Stop: stopList(in.Stop)}
	if in.MaxCompletionTokens > 0 {
		r.MaxTokens = in.MaxCompletionTokens
	}
	if in.StreamOptions != nil {
		r.IncludeUsage = in.StreamOptions.IncludeUsage
	}
	if in.ReasoningEffort != "" {
		on := in.ReasoningEffort != "none"
		r.Think = &on
	}
	if len(in.ResponseFormat) > 0 {
		r.Format = schemaFromResponseFormat(in.ResponseFormat)
	}
	toolNames := map[string]string{}
	for _, m := range in.Messages {
		text, images, err := oaContent(m.Content)
		if err != nil {
			return chatRequest{}, err
		}
		switch m.Role {
		case "system", "developer":
			r.Messages = append(r.Messages, chatMessage{Role: "system", Content: text})
		case "user":
			r.Messages = append(r.Messages, chatMessage{Role: "user", Content: text, Images: images})
		case "assistant":
			for _, c := range m.ToolCalls {
				toolNames[c.ID] = c.Function.Name
				text += fmt.Sprintf("\n[called %s %s]", c.Function.Name, c.Function.Arguments)
			}
			r.Messages = append(r.Messages, chatMessage{Role: "assistant", Content: strings.TrimSpace(text)})
		case "tool":
			name := toolNames[m.ToolCallID]
			if name == "" {
				name = m.Name
			}
			r.Messages = append(r.Messages, chatMessage{Role: "user", Content: fmt.Sprintf("Result of %s: %s", name, text)})
		default:
			return chatRequest{}, bad("unknown message role %q", m.Role)
		}
	}
	return r, nil
}

// oaContent reads a message's content, which is a string or a list of typed parts. Images must be
// data: URLs; noskill doesn't fetch remote URLs on a client's behalf.
func oaContent(raw json.RawMessage) (string, [][]byte, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, bad("message content must be a string or a list of parts")
	}
	var text []string
	var images [][]byte
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text":
			text = append(text, p.Text)
		case "image_url", "input_image":
			img, err := decodeDataURL(p.ImageURL.URL)
			if err != nil {
				text = append(text, "[an image was omitted: "+err.Error()+"]")
				continue
			}
			images = append(images, img)
		}
	}
	return strings.Join(text, "\n"), images, nil
}

func decodeDataURL(u string) ([]byte, error) {
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return nil, errors.New("only data: image URLs are supported")
	}
	_, data, ok := strings.Cut(rest, ";base64,")
	if !ok {
		return nil, errors.New("the image isn't base64")
	}
	return decodeImage(data)
}

func decodeImage(b64 string) ([]byte, error) {
	if base64.StdEncoding.DecodedLen(len(b64)) > maxImageBytes {
		return nil, errors.New("the image is larger than 20 MB")
	}
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, errors.New("the image isn't valid base64")
	}
	return b, nil
}

func stopList(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil && one != "" {
		return []string{one}
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		if len(many) > 4 {
			many = many[:4]
		}
		return many
	}
	return nil
}

// schemaFromResponseFormat extracts the JSON Schema from an OpenAI response_format.
func schemaFromResponseFormat(raw json.RawMessage) json.RawMessage {
	var rf struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	}
	if json.Unmarshal(raw, &rf) != nil {
		return nil
	}
	switch rf.Type {
	case "json_schema":
		return rf.JSONSchema.Schema
	case "json_object":
		return json.RawMessage(`{"type":"object"}`)
	}
	return nil
}

// ---- Ollama ----

type olOptions struct {
	Temperature *float64        `json:"temperature"`
	TopP        *float64        `json:"top_p"`
	Seed        *int            `json:"seed"`
	NumPredict  int             `json:"num_predict"`
	Stop        json.RawMessage `json:"stop"`
}

func olThink(raw json.RawMessage) *bool {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return &b
	}
	var s string // newer Ollama accepts "low" | "medium" | "high"
	if json.Unmarshal(raw, &s) == nil {
		on := s != "" && s != "none"
		return &on
	}
	return nil
}

func olFormat(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil { // "json"
		if s == "json" {
			return json.RawMessage(`{"type":"object"}`)
		}
		return nil
	}
	return raw
}

func parseOllamaChat(body []byte) (chatRequest, error) {
	var in struct {
		Model    string `json:"model"`
		Messages []struct {
			Role      string   `json:"role"`
			Content   string   `json:"content"`
			Images    []string `json:"images"`
			ToolName  string   `json:"tool_name"`
			ToolCalls []struct {
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Stream  *bool             `json:"stream"`
		Options olOptions         `json:"options"`
		Think   json.RawMessage   `json:"think"`
		Format  json.RawMessage   `json:"format"`
		Tools   []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return chatRequest{}, bad("the request body isn't valid JSON: %v", err)
	}
	if in.Model == "" {
		return chatRequest{}, bad(`"model" is required`)
	}
	r := chatRequest{Model: in.Model, Stream: in.Stream == nil || *in.Stream, Temperature: in.Options.Temperature, TopP: in.Options.TopP,
		Seed: in.Options.Seed, MaxTokens: in.Options.NumPredict, Stop: stopList(in.Options.Stop), Think: olThink(in.Think),
		Format: olFormat(in.Format), HasTools: len(in.Tools) > 0}
	for _, m := range in.Messages {
		text := m.Content
		for _, c := range m.ToolCalls {
			text += fmt.Sprintf("\n[called %s %s]", c.Function.Name, string(c.Function.Arguments))
		}
		cm := chatMessage{Role: m.Role, Content: strings.TrimSpace(text)}
		switch m.Role {
		case "system", "user", "assistant":
		case "tool":
			cm = chatMessage{Role: "user", Content: fmt.Sprintf("Result of %s: %s", m.ToolName, m.Content)}
		default:
			return chatRequest{}, bad("unknown message role %q", m.Role)
		}
		for _, img := range m.Images {
			b, err := decodeImage(img)
			if err != nil {
				return chatRequest{}, bad("%v", err)
			}
			cm.Images = append(cm.Images, b)
		}
		r.Messages = append(r.Messages, cm)
	}
	if len(r.Messages) == 0 {
		return chatRequest{}, bad(`"messages" is required`)
	}
	return r, nil
}

func parseOllamaGenerate(body []byte) (chatRequest, error) {
	var in struct {
		Model   string          `json:"model"`
		Prompt  string          `json:"prompt"`
		System  string          `json:"system"`
		Images  []string        `json:"images"`
		Stream  *bool           `json:"stream"`
		Raw     bool            `json:"raw"`
		Options olOptions       `json:"options"`
		Think   json.RawMessage `json:"think"`
		Format  json.RawMessage `json:"format"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return chatRequest{}, bad("the request body isn't valid JSON: %v", err)
	}
	if in.Model == "" {
		return chatRequest{}, bad(`"model" is required`)
	}
	r := chatRequest{Model: in.Model, Stream: in.Stream == nil || *in.Stream, Temperature: in.Options.Temperature, TopP: in.Options.TopP,
		Seed: in.Options.Seed, MaxTokens: in.Options.NumPredict, Stop: stopList(in.Options.Stop), Think: olThink(in.Think),
		Format: olFormat(in.Format), Raw: in.Raw}
	if in.System != "" {
		r.Messages = append(r.Messages, chatMessage{Role: "system", Content: in.System})
	}
	um := chatMessage{Role: "user", Content: in.Prompt}
	for _, img := range in.Images {
		b, err := decodeImage(img)
		if err != nil {
			return chatRequest{}, bad("%v", err)
		}
		um.Images = append(um.Images, b)
	}
	r.Messages = append(r.Messages, um)
	return r, nil
}

// backendMessages converts to the engine's message type.
func backendMessages(ms []chatMessage) []backends.Message {
	out := make([]backends.Message, len(ms))
	for i, m := range ms {
		out[i] = backends.Message{Role: backends.Role(m.Role), Content: m.Content, Images: m.Images}
	}
	return out
}

func (r chatRequest) options() *backends.Options {
	if r.Temperature == nil && r.TopP == nil && r.Seed == nil && r.MaxTokens == 0 && len(r.Stop) == 0 {
		return nil
	}
	return &backends.Options{Temperature: r.Temperature, TopP: r.TopP, Seed: r.Seed, NumPredict: r.MaxTokens, Stop: r.Stop}
}

func (r chatRequest) lastUser() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role == "user" {
			return r.Messages[i].Content
		}
	}
	return ""
}
