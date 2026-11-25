// Package musterapproval asks muster, as the caller, how an agent's tool list
// resolves: filter_tools with the reporting-only agentTools argument, which
// names per tool the entry that decides the agent's setting, the
// connector's own setting and the connectors behind it. agent-manager needs
// it to tell an Allow over a connector's Ask (which only a connector admin
// grants) from one that changes nothing.
package musterapproval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/mustermcp"
)

const (
	// FilterTool is muster's meta-tool.
	FilterTool = "filter_tools"
	// pageSize is the page agent-manager reads; it pages until muster says
	// the answer is complete.
	pageSize = 500
	// maxPages bounds the paging of one answer.
	maxPages = 40
)

// Caller runs one of muster's meta-tools as the caller.
type Caller interface {
	MetaCall(ctx context.Context, bearer, tool string, args map[string]any) (*mustermcp.Result, error)
}

// Reporter is agents.ApprovalReporter over muster's MCP endpoint.
type Reporter struct {
	muster Caller
}

// New is the reporter over muster's meta-tools.
func New(c Caller) *Reporter { return &Reporter{muster: c} }

// page is the part of filter_tools' answer the reporter reads.
type page struct {
	Truncated bool `json:"truncated"`
	Tools     []struct {
		Name     string `json:"name"`
		Approval *struct {
			Setting     string `json:"setting"`
			Source      string `json:"source"`
			Entry       string `json:"entry"`
			Unavailable string `json:"unavailable"`
			Admin       *struct {
				Setting string `json:"setting"`
				Source  string `json:"source"`
			} `json:"admin"`
		} `json:"approval"`
		Connectors []agents.Connector `json:"connectors"`
	} `json:"tools"`
}

// AgentTools implements agents.ApprovalReporter.
func (r *Reporter) AgentTools(ctx context.Context, token string, q agents.AgentToolsQuery) ([]agents.ReportedTool, error) {
	agentTools := map[string]any{"tools": q.Tools}
	if q.Default != "" {
		agentTools["default"] = q.Default
	}
	// The list goes as JSON values (the chart's shape).
	raw, err := json.Marshal(agentTools)
	if err != nil {
		return nil, err
	}
	var arg map[string]any
	if err := json.Unmarshal(raw, &arg); err != nil {
		return nil, err
	}
	var out []agents.ReportedTool
	for p, offset := 0, 0; p < maxPages; p++ {
		res, err := r.muster.MetaCall(ctx, token, FilterTool, map[string]any{"agentTools": arg, "limit": pageSize, "offset": offset})
		if err != nil {
			var toolErr *mustermcp.ToolError
			if errors.As(err, &toolErr) {
				return nil, fmt.Errorf("%w: muster's filter_tools refused the agent's tool list: %s", agents.ErrApprovalUnavailable, toolErr.Message)
			}
			return nil, err
		}
		body := res.JSON()
		if body == nil {
			// An empty catalogue answers in plain text.
			return out, nil
		}
		var got page
		if err := json.Unmarshal(body, &got); err != nil {
			return nil, fmt.Errorf("%w: muster's filter_tools answer is malformed: %v", agents.ErrApprovalUnavailable, err)
		}
		for _, t := range got.Tools {
			rt := agents.ReportedTool{Name: t.Name, Connectors: t.Connectors}
			if t.Approval != nil {
				rt.Entry = t.Approval.Entry
				if t.Approval.Admin != nil {
					rt.Admin = t.Approval.Admin.Setting
				}
				if rt.Entry != "" && t.Approval.Admin == nil {
					return nil, fmt.Errorf("%w: muster reports tool %s without its connector's own setting (admin): it does not evaluate agent tool lists yet", agents.ErrApprovalUnavailable, t.Name)
				}
			}
			out = append(out, rt)
		}
		if !got.Truncated || len(got.Tools) == 0 {
			return out, nil
		}
		offset += len(got.Tools)
	}
	return nil, fmt.Errorf("%w: muster's filter_tools answer did not end after %d pages", agents.ErrApprovalUnavailable, maxPages)
}
