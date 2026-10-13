package agents

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// The views get_session and get_session_turn answer a conversation in.
const (
	// DetailSummary is per turn the user's message, the agent's reply and
	// one stub per tool call.
	DetailSummary = "summary"
	// DetailTranscript adds every message, tool calls with clipped
	// arguments and tool results as stubs.
	DetailTranscript = "transcript"
	// DetailFull is the A2A objects as kagent answers them (proto3 JSON).
	DetailFull = "full"
)

const (
	// DefaultSessionMaxBytes, MinSessionMaxBytes and MaxSessionMaxBytes bound
	// the answer of get_session and get_session_turn.
	DefaultSessionMaxBytes = 100_000
	MinSessionMaxBytes     = 4096
	MaxSessionMaxBytes     = 1 << 20
	// maxArgsBytes clips a tool call's arguments in a transcript.
	maxArgsBytes = 500
	// maxUnwrapDepth bounds how many envelopes a tool response is unwrapped
	// through.
	maxUnwrapDepth = 6
)

// Turn is one turn of a conversation, one A2A task, indexed from 0 oldest
// first. Messages is set in the transcript view only.
type Turn struct {
	Index     int            `json:"index"`
	TaskID    string         `json:"taskId"`
	State     string         `json:"state"`
	Timestamp string         `json:"timestamp,omitempty"`
	User      string         `json:"user,omitempty"`
	Reply     string         `json:"reply,omitempty"`
	ToolCalls []ToolCallStub `json:"toolCalls,omitempty"`
	Messages  []TurnMessage  `json:"messages,omitempty"`
	// Clipped says the turn alone exceeded the budget: its messages and tool
	// calls were dropped and its texts clipped.
	Clipped bool `json:"clipped,omitempty"`
}

// ToolCallStub names one tool call of a turn. Index addresses it in
// get_session_turn's toolCall.
type ToolCallStub struct {
	Index   int    `json:"index"`
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Error   bool   `json:"error,omitempty"`
	Pending bool   `json:"pending,omitempty"`
}

// TurnMessage is one message of a turn in the transcript view. Index is its
// position in the turn, as get_session_turn's fromMessage counts.
type TurnMessage struct {
	Index   int        `json:"index"`
	Role    string     `json:"role"`
	Parts   []PartView `json:"parts,omitempty"`
	Clipped bool       `json:"clipped,omitempty"`
}

// PartView is one part of a message: exactly one field is set.
type PartView struct {
	Text       string          `json:"text,omitempty"`
	ToolCall   *ToolCallView   `json:"toolCall,omitempty"`
	ToolResult *ToolResultView `json:"toolResult,omitempty"`
	Other      *OtherPartView  `json:"other,omitempty"`
}

// ToolCallView is a tool call with its arguments as JSON, clipped.
type ToolCallView struct {
	Index int    `json:"index"`
	ID    string `json:"id,omitempty"`
	Name  string `json:"name"`
	Args  string `json:"args,omitempty"`
}

// ToolResultView stands for a tool response: the call it answers, whether it
// failed and its size. get_session_turn's toolCall reads it.
type ToolResultView struct {
	Index int    `json:"index"`
	ID    string `json:"id,omitempty"`
	Name  string `json:"name"`
	Error bool   `json:"error,omitempty"`
	Bytes int    `json:"bytes"`
}

// OtherPartView stands for a part that is neither text nor a tool call or
// result: a file (raw or url) or other data.
type OtherPartView struct {
	Kind      string `json:"kind"`
	MediaType string `json:"mediaType,omitempty"`
	Filename  string `json:"filename,omitempty"`
	URL       string `json:"url,omitempty"`
	Bytes     int    `json:"bytes,omitempty"`
}

// ToolCallDetail is one tool call whole: its arguments and its response
// unwrapped from the envelopes it travelled in. ArgsText and ResponseText
// replace Args and Response when they had to be clipped.
type ToolCallDetail struct {
	Index        int             `json:"index"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name"`
	Pending      bool            `json:"pending,omitempty"`
	Error        bool            `json:"error,omitempty"`
	Args         json.RawMessage `json:"args,omitempty"`
	ArgsText     string          `json:"argsText,omitempty"`
	Response     json.RawMessage `json:"response,omitempty"`
	ResponseText string          `json:"responseText,omitempty"`
}

// turn is a task read for the views: its messages and its tool calls.
type turn struct {
	index    int
	task     *a2apb.Task
	messages []*a2apb.Message
	calls    []*toolCall
	// callOf is the tool call a function_call or function_response part
	// belongs to.
	callOf map[*a2apb.Part]*toolCall
}

type toolCall struct {
	index    int
	id, name string
	args     *structpb.Value
	response *structpb.Value
	answered bool
	failed   bool
}

// readTurn parses task, the index-th turn of its conversation.
func readTurn(index int, task *a2apb.Task) *turn {
	t := &turn{index: index, task: task, messages: turnMessages(task), callOf: map[*a2apb.Part]*toolCall{}}
	for _, msg := range t.messages {
		for _, part := range msg.GetParts() {
			switch partType(part) {
			case "function_call":
				id, name := dataString(part, "id"), dataString(part, "name")
				call := &toolCall{index: len(t.calls), id: id, name: name, args: part.GetData().GetStructValue().GetFields()["args"]}
				t.calls = append(t.calls, call)
				t.callOf[part] = call
			case "function_response":
				id, name := dataString(part, "id"), dataString(part, "name")
				call := t.unanswered(id, name)
				if call == nil {
					call = &toolCall{index: len(t.calls), id: id, name: name}
					t.calls = append(t.calls, call)
				}
				call.answered = true
				call.response = part.GetData().GetStructValue().GetFields()["response"]
				_, call.failed = unwrapResponse(call.response.AsInterface())
				t.callOf[part] = call
			}
		}
	}
	return t
}

// unanswered is the first call without a response that id (or, without an
// id, name) names.
func (t *turn) unanswered(id, name string) *toolCall {
	for _, call := range t.calls {
		if call.answered {
			continue
		}
		if (id != "" && call.id == id) || (id == "" && call.name == name) {
			return call
		}
	}
	return nil
}

// turnMessages is a task's conversation: its history and, unless the history
// already holds it, its status message (the agent's latest word).
func turnMessages(task *a2apb.Task) []*a2apb.Message {
	history := task.GetHistory()
	status := task.GetStatus().GetMessage()
	if status == nil {
		return history
	}
	for _, msg := range history {
		if status.GetMessageId() != "" && msg.GetMessageId() == status.GetMessageId() {
			return history
		}
		if status.GetMessageId() == "" && proto.Equal(msg, status) {
			return history
		}
	}
	return append(slices.Clone(history), status)
}

// partType is a data part's ADK type (function_call, function_response, …),
// read as the ADK reads it: adk_type, else kagent_type.
func partType(part *a2apb.Part) string {
	if part.GetData() == nil {
		return ""
	}
	fields := part.GetMetadata().GetFields()
	for _, key := range []string{"adk_type", "kagent_type"} {
		if v, ok := fields[key]; ok {
			return v.GetStringValue()
		}
	}
	return ""
}

func dataString(part *a2apb.Part, key string) string {
	return part.GetData().GetStructValue().GetFields()[key].GetStringValue()
}

func (t *turn) state() string { return t.task.GetStatus().GetState().String() }

func (t *turn) timestamp() string {
	ts := t.task.GetStatus().GetTimestamp()
	if ts == nil {
		return ""
	}
	return ts.AsTime().UTC().Format(time.RFC3339)
}

// stubs are the turn's tool calls by name.
func (t *turn) stubs() []ToolCallStub {
	var out []ToolCallStub
	for _, call := range t.calls {
		out = append(out, ToolCallStub{Index: call.index, ID: call.id, Name: call.name, Error: call.failed, Pending: !call.answered})
	}
	return out
}

// summary is the turn in the summary view: the first user message, the
// status message and the tool call stubs.
func (t *turn) summary() Turn {
	out := Turn{
		Index: t.index, TaskID: t.task.GetId(), State: t.state(), Timestamp: t.timestamp(),
		Reply: messageText(t.task.GetStatus().GetMessage()), ToolCalls: t.stubs(),
	}
	for _, msg := range t.messages {
		if msg.GetRole() == a2apb.Role_ROLE_USER {
			out.User = messageText(msg)
			break
		}
	}
	return out
}

// transcript is the summary with every message.
func (t *turn) transcript() Turn {
	out := t.summary()
	for i := range t.messages {
		out.Messages = append(out.Messages, t.messageView(i))
	}
	return out
}

func (t *turn) messageView(i int) TurnMessage {
	msg := t.messages[i]
	out := TurnMessage{Index: i, Role: strings.ToLower(strings.TrimPrefix(msg.GetRole().String(), "ROLE_"))}
	for _, part := range msg.GetParts() {
		out.Parts = append(out.Parts, t.partView(part))
	}
	return out
}

func (t *turn) partView(part *a2apb.Part) PartView {
	if call, ok := t.callOf[part]; ok {
		if partType(part) == "function_call" {
			return PartView{ToolCall: &ToolCallView{Index: call.index, ID: call.id, Name: call.name, Args: clipText(jsonText(call.args), maxArgsBytes)}}
		}
		return PartView{ToolResult: &ToolResultView{Index: call.index, ID: call.id, Name: call.name, Error: call.failed, Bytes: len(jsonText(call.response))}}
	}
	switch content := part.GetContent().(type) {
	case *a2apb.Part_Text:
		return PartView{Text: content.Text}
	case *a2apb.Part_Raw:
		return PartView{Other: &OtherPartView{Kind: "raw", MediaType: part.GetMediaType(), Filename: part.GetFilename(), Bytes: len(content.Raw)}}
	case *a2apb.Part_Url:
		return PartView{Other: &OtherPartView{Kind: "url", MediaType: part.GetMediaType(), Filename: part.GetFilename(), URL: content.Url}}
	default:
		return PartView{Other: &OtherPartView{Kind: "data", MediaType: part.GetMediaType(), Filename: part.GetFilename(), Bytes: len(jsonText(part.GetData()))}}
	}
}

// toolCallDetail is call whole, within budget bytes.
func (t *turn) toolCallDetail(call *toolCall, budget int) (ToolCallDetail, bool) {
	out := ToolCallDetail{Index: call.index, ID: call.id, Name: call.name, Pending: !call.answered, Error: call.failed}
	if call.args != nil {
		out.Args = json.RawMessage(jsonText(call.args))
	}
	if call.response != nil {
		inner, _ := unwrapResponse(call.response.AsInterface())
		if data, err := json.Marshal(inner); err == nil {
			out.Response = data
		}
	}
	if jsonSize(out) <= budget {
		return out, false
	}
	if len(out.Args) > budget/4 {
		out.ArgsText, out.Args = clipText(string(out.Args), budget/4), nil
	}
	responseText := string(out.Response)
	var s string
	if json.Unmarshal(out.Response, &s) == nil {
		responseText = s
	}
	out.Response = nil
	for limit := budget - jsonSize(out); limit > 0; limit /= 2 {
		out.ResponseText = clipText(responseText, limit)
		if jsonSize(out) <= budget {
			break
		}
	}
	return out, true
}

// call is the turn's tool call ref names: its index or its id.
func (t *turn) call(ref string) *toolCall {
	for _, call := range t.calls {
		if fmt.Sprint(call.index) == ref {
			return call
		}
	}
	for _, call := range t.calls {
		if call.id != "" && call.id == ref {
			return call
		}
	}
	return nil
}

// unwrapResponse decodes a tool response through the envelopes it travels
// in: a runtime's {result, isError}, an MCP CallToolResult with one text
// content, and JSON encoded as a string (muster nests whole MCP results that
// way). It answers the innermost value and whether any layer reported an
// error.
func unwrapResponse(v any) (any, bool) {
	failed := false
	for range maxUnwrapDepth {
		next, layerFailed, ok := unwrapOnce(v)
		failed = failed || layerFailed
		if !ok {
			break
		}
		v = next
	}
	if m, ok := v.(map[string]any); ok && m["error"] != nil && m["error"] != "" {
		failed = true
	}
	return v, failed
}

func unwrapOnce(v any) (next any, failed, ok bool) {
	switch t := v.(type) {
	case string:
		s := strings.TrimSpace(t)
		if s == "" || (s[0] != '{' && s[0] != '[') {
			return v, false, false
		}
		var out any
		if json.Unmarshal([]byte(s), &out) != nil {
			return v, false, false
		}
		return out, false, true
	case map[string]any:
		failed, _ = t["isError"].(bool)
		if content, isList := t["content"].([]any); isList && len(content) == 1 && onlyKeys(t, "content", "isError", "structuredContent", "_meta") {
			if item, isMap := content[0].(map[string]any); isMap && item["type"] == "text" {
				if text, isText := item["text"].(string); isText {
					return text, failed, true
				}
			}
		}
		if result, has := t["result"]; has && onlyKeys(t, "result", "isError") {
			return result, failed, true
		}
		return v, failed, false
	}
	return v, false, false
}

func onlyKeys(m map[string]any, keys ...string) bool {
	for k := range m {
		if !slices.Contains(keys, k) {
			return false
		}
	}
	return true
}

func messageText(msg *a2apb.Message) string {
	var texts []string
	for _, part := range msg.GetParts() {
		if text := part.GetText(); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

func jsonText(m proto.Message) string {
	if m == nil {
		return ""
	}
	data, err := protoJSON.Marshal(m)
	if err != nil {
		return ""
	}
	return string(data)
}

// jsonSize is v's size in the answer.
func jsonSize(v any) int {
	data, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(data)
}

// clipText cuts s to about n bytes on a rune boundary and says how much it
// cut.
func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := max(n-24, 0)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("…[%d bytes clipped]", len(s)-cut)
}

// fitTurn cuts a turn that alone exceeds budget: it drops the messages, then
// the tool calls, then clips the texts until it fits. It answers false when
// even that is too big.
func fitTurn(in Turn, budget int) (Turn, bool) {
	out := in
	out.Clipped, out.Messages = true, nil
	if jsonSize(out) <= budget {
		return out, true
	}
	out.ToolCalls = nil
	rest := budget - jsonSize(Turn{Index: out.Index, TaskID: out.TaskID, State: out.State, Timestamp: out.Timestamp, Clipped: true})
	for userMax, replyMax := rest/4, rest*3/4; replyMax > 0; userMax, replyMax = userMax/2, replyMax/2 {
		out.User, out.Reply = clipText(in.User, userMax), clipText(in.Reply, replyMax)
		if jsonSize(out) <= budget {
			return out, true
		}
	}
	return out, false
}

// packNewest keeps the newest items that fit budget bytes, oldest first, and
// counts the older ones it left out. When even the newest does not fit, fit
// may cut it down; without fit it is left out too.
func packNewest[T any](items []T, budget int, fit func(T, int) (T, bool)) (kept []T, omitted int) {
	used := 0
	i := len(items) - 1
	for ; i >= 0; i-- {
		size := jsonSize(items[i]) + 1
		if used+size <= budget {
			kept = append(kept, items[i])
			used += size
			continue
		}
		if len(kept) == 0 && fit != nil {
			if cut, ok := fit(items[i], budget-1); ok {
				kept = append(kept, cut)
				i--
			}
		}
		break
	}
	slices.Reverse(kept)
	return kept, i + 1
}
