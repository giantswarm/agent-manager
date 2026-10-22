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
	MaxSessionMaxBytes     = 32 << 20
	// maxArgsBytes clips a tool call's arguments in a transcript.
	maxArgsBytes = 500
	// maxUnwrapDepth bounds how many envelopes a tool response is unwrapped
	// through.
	maxUnwrapDepth = 6
)

// Turn is one turn of a conversation, one A2A task, indexed from 0 oldest
// first. The summary view sets User, Reply and ToolCalls, the transcript
// view Messages, which hold all of them. Artifacts counts the task's
// artifacts.
type Turn struct {
	Index     int            `json:"index"`
	TaskID    string         `json:"taskId"`
	State     string         `json:"state"`
	Timestamp string         `json:"timestamp,omitempty"`
	User      string         `json:"user,omitempty"`
	Reply     string         `json:"reply,omitempty"`
	ToolCalls []ToolCallStub `json:"toolCalls,omitempty"`
	Artifacts int            `json:"artifacts,omitempty"`
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

// TurnMessage is one message of a turn in the transcript view, or one of the
// task's artifacts after them (role "artifact", with its name). Index is its
// position in the turn, as get_session_turn's fromMessage counts.
type TurnMessage struct {
	Index   int        `json:"index"`
	Role    string     `json:"role"`
	Name    string     `json:"name,omitempty"`
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
// status message (else the artifacts' text, where a runtime may answer) and
// the tool call stubs.
func (t *turn) summary() Turn {
	out := t.header()
	out.Reply, out.ToolCalls = messageText(t.task.GetStatus().GetMessage()), t.stubs()
	if out.Reply == "" {
		var texts []string
		for _, artifact := range t.task.GetArtifacts() {
			if text := partsText(artifact.GetParts()); text != "" {
				texts = append(texts, text)
			}
		}
		out.Reply = strings.Join(texts, "\n")
	}
	for _, msg := range t.messages {
		if msg.GetRole() == a2apb.Role_ROLE_USER {
			out.User = messageText(msg)
			break
		}
	}
	return out
}

// transcript is the turn with every message and artifact, which hold what
// the summary picks from them.
func (t *turn) transcript() Turn {
	out := t.header()
	for i := range t.items() {
		out.Messages = append(out.Messages, t.messageView(i))
	}
	return out
}

func (t *turn) header() Turn {
	return Turn{Index: t.index, TaskID: t.task.GetId(), State: t.state(), Timestamp: t.timestamp(), Artifacts: len(t.task.GetArtifacts())}
}

// items counts the turn's messages and, after them, its artifacts.
func (t *turn) items() int { return len(t.messages) + len(t.task.GetArtifacts()) }

func (t *turn) messageView(i int) TurnMessage {
	if i >= len(t.messages) {
		artifact := t.task.GetArtifacts()[i-len(t.messages)]
		out := TurnMessage{Index: i, Role: "artifact", Name: artifact.GetName()}
		for _, part := range artifact.GetParts() {
			out.Parts = append(out.Parts, t.partView(part))
		}
		return out
	}
	msg := t.messages[i]
	out := TurnMessage{Index: i, Role: strings.ToLower(strings.TrimPrefix(msg.GetRole().String(), "ROLE_"))}
	for _, part := range msg.GetParts() {
		out.Parts = append(out.Parts, t.partView(part))
	}
	return out
}

// rawItem is the i-th message as proto3 JSON, or an artifact as
// {"artifact": …}.
func (t *turn) rawItem(i int) (json.RawMessage, error) {
	if i < len(t.messages) {
		return marshalProto(t.messages[i])
	}
	raw, err := marshalProto(t.task.GetArtifacts()[i-len(t.messages)])
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]json.RawMessage{"artifact": raw})
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
	// The smaller of arguments and response stays whole when it fits half the
	// room; the other is clipped to what is left.
	args, response := out.Args, out.Response
	out.Args, out.Response = nil, nil
	room := budget - jsonSize(out)
	argsShare, responseShare := room/2, room/2
	switch {
	case len(args) <= room/2:
		argsShare, responseShare = len(args), room-len(args)
	case len(response) <= room/2:
		argsShare, responseShare = room-len(response), len(response)
	}
	if len(args) <= argsShare {
		out.Args = args
	}
	if len(response) <= responseShare {
		out.Response = response
	}
	for range 8 {
		if out.Args == nil && len(args) > 0 {
			out.ArgsText = clipText(jsonString(args), argsShare)
		}
		if out.Response == nil && len(response) > 0 {
			out.ResponseText = clipText(jsonString(response), responseShare)
		}
		over := jsonSize(out) - budget
		if over <= 0 {
			break
		}
		// Escaping grew the clipped texts: take the excess off them.
		if out.ResponseText != "" {
			responseShare = max(responseShare-over, 0)
		} else {
			argsShare = max(argsShare-over, 0)
		}
	}
	return out, true
}

// jsonString is a JSON string's value, or any other JSON as its text.
func jsonString(data json.RawMessage) string {
	var s string
	if json.Unmarshal(data, &s) == nil {
		return s
	}
	return string(data)
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
	if m, ok := v.(map[string]any); ok && reportsError(m["error"]) {
		failed = true
	}
	return v, failed
}

// reportsError says whether an error field holds an error: true, a message
// or an object with content, not false, "", null or {}.
func reportsError(v any) bool {
	switch e := v.(type) {
	case bool:
		return e
	case string:
		return e != ""
	case map[string]any:
		return len(e) > 0
	case []any:
		return len(e) > 0
	}
	return false
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

func messageText(msg *a2apb.Message) string { return partsText(msg.GetParts()) }

func partsText(parts []*a2apb.Part) string {
	var texts []string
	for _, part := range parts {
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

// clipText cuts s to at most n bytes on a rune boundary and says how much it
// cut, when n leaves room to say so.
func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	n = max(n, 0)
	// The marker for cutting all of s is the longest one.
	reserve := len(clipMarker(len(s)))
	if n < reserve {
		reserve = 0
	}
	cut := n - reserve
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if reserve == 0 {
		return s[:cut]
	}
	return s[:cut] + clipMarker(len(s)-cut)
}

func clipMarker(clipped int) string { return fmt.Sprintf("…[%d bytes clipped]", clipped) }

// fitTurn cuts a turn's summary that alone exceeds budget: it drops the tool
// calls, then clips the texts until it fits. It answers false when even that
// is too big.
func fitTurn(in Turn, budget int) (Turn, bool) {
	out := in
	out.Clipped = true
	if jsonSize(out) <= budget {
		return out, true
	}
	out.ToolCalls = nil
	rest := budget - jsonSize(Turn{Index: out.Index, TaskID: out.TaskID, State: out.State, Timestamp: out.Timestamp, Artifacts: out.Artifacts, Clipped: true})
	for userMax, replyMax := rest/4, rest*3/4; replyMax > 0; userMax, replyMax = userMax/2, replyMax/2 {
		out.User, out.Reply = clipText(in.User, userMax), clipText(in.Reply, replyMax)
		if jsonSize(out) <= budget {
			return out, true
		}
	}
	return out, false
}
