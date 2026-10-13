package kagenttest

import (
	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// Text is a text part.
func Text(text string) *a2apb.Part {
	return &a2apb.Part{Content: &a2apb.Part_Text{Text: text}}
}

// ToolCall is a function_call data part as an ADK runtime writes it, its type
// under the metadata key <prefix>type (prefix "adk_" or "kagent_").
func ToolCall(prefix, id, name string, args map[string]any) *a2apb.Part {
	return dataPart(prefix, "function_call", map[string]any{"id": id, "name": name, "args": args})
}

// ToolResult is the function_response data part answering ToolCall id.
func ToolResult(prefix, id, name string, response map[string]any) *a2apb.Part {
	return dataPart(prefix, "function_response", map[string]any{"id": id, "name": name, "response": response})
}

// Message is a message of role with parts.
func Message(role a2apb.Role, parts ...*a2apb.Part) *a2apb.Message {
	return &a2apb.Message{Role: role, Parts: parts}
}

func dataPart(prefix, typ string, data map[string]any) *a2apb.Part {
	value, err := structpb.NewValue(data)
	if err != nil {
		panic(err)
	}
	return &a2apb.Part{
		Content:  &a2apb.Part_Data{Data: value},
		Metadata: &structpb.Struct{Fields: map[string]*structpb.Value{prefix + "type": structpb.NewStringValue(typ)}},
	}
}
