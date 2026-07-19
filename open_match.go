package main

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"open-match.dev/open-match/pkg/pb"
)

var errOpenMatchNotConfigured = errors.New("Open Match is not configured for this cluster; set AGONES_MCP_OPEN_MATCH_FRONTEND (or AGONES_MCP_OPEN_MATCH_FRONTENDS in multi-cluster mode)")

// Sanity ceilings, not real matchmaking limits.
const (
	maxTicketTags           = 32
	maxTicketSearchEntries  = 32
	maxTicketFieldStringLen = 256
)

func validateTicketFields(in OpenMatchCreateTicketInput) error {
	if len(in.Tags) > maxTicketTags {
		return fmt.Errorf("tags: %d exceeds the limit of %d", len(in.Tags), maxTicketTags)
	}
	for _, tag := range in.Tags {
		if utf8.RuneCountInString(tag) > maxTicketFieldStringLen {
			return fmt.Errorf("tags: %q exceeds %d characters", tag, maxTicketFieldStringLen)
		}
	}
	if len(in.StringArgs) > maxTicketSearchEntries {
		return fmt.Errorf("stringArgs: %d entries exceeds the limit of %d", len(in.StringArgs), maxTicketSearchEntries)
	}
	for k, v := range in.StringArgs {
		if utf8.RuneCountInString(k) > maxTicketFieldStringLen || utf8.RuneCountInString(v) > maxTicketFieldStringLen {
			return fmt.Errorf("stringArgs[%q]: key or value exceeds %d characters", k, maxTicketFieldStringLen)
		}
	}
	if len(in.DoubleArgs) > maxTicketSearchEntries {
		return fmt.Errorf("doubleArgs: %d entries exceeds the limit of %d", len(in.DoubleArgs), maxTicketSearchEntries)
	}
	for k := range in.DoubleArgs {
		if utf8.RuneCountInString(k) > maxTicketFieldStringLen {
			return fmt.Errorf("doubleArgs[%q]: key exceeds %d characters", k, maxTicketFieldStringLen)
		}
	}
	return nil
}

type OpenMatchTicketOutput struct {
	TicketID   string   `json:"ticketId"`
	Tags       []string `json:"tags,omitempty"`
	Assigned   bool     `json:"assigned"`
	Connection string   `json:"connection,omitempty"`
}

func ticketOutput(t *pb.Ticket) OpenMatchTicketOutput {
	out := OpenMatchTicketOutput{TicketID: t.GetId()}
	if sf := t.GetSearchFields(); sf != nil {
		out.Tags = sf.GetTags()
	}
	if a := t.GetAssignment(); a != nil && a.GetConnection() != "" {
		out.Assigned = true
		out.Connection = a.GetConnection()
	}
	return out
}

type OpenMatchCreateTicketInput struct {
	Tags       []string           `json:"tags,omitempty" jsonschema:"Presence tags for matchmaking pools to select on, e.g. mode.demo (max 32 tags, 256 characters each)"`
	StringArgs map[string]string  `json:"stringArgs,omitempty" jsonschema:"String search fields, matched on equality (max 32 entries, 256 characters per key/value)"`
	DoubleArgs map[string]float64 `json:"doubleArgs,omitempty" jsonschema:"Numeric search fields, matched on range (max 32 entries, 256 characters per key)"`
	Cluster    string             `json:"cluster,omitempty" jsonschema:"Cluster to target; omit for the default cluster"`
}

// Poll openMatchTicketStatus to see when the ticket gets an Assignment.
func (s *server) openMatchCreateTicket(ctx context.Context, req *mcp.CallToolRequest, in OpenMatchCreateTicketInput) (*mcp.CallToolResult, OpenMatchTicketOutput, error) {
	if err := validateTicketFields(in); err != nil {
		return nil, OpenMatchTicketOutput{}, err
	}
	cl, err := s.c.get(in.Cluster)
	if err != nil {
		return nil, OpenMatchTicketOutput{}, err
	}
	if cl.omFrontend == nil {
		return nil, OpenMatchTicketOutput{}, errOpenMatchNotConfigured
	}
	ticket := &pb.Ticket{
		SearchFields: &pb.SearchFields{
			Tags:       in.Tags,
			StringArgs: in.StringArgs,
			DoubleArgs: in.DoubleArgs,
		},
	}
	created, err := cl.omFrontend.CreateTicket(ctx, &pb.CreateTicketRequest{Ticket: ticket})
	if err != nil {
		return nil, OpenMatchTicketOutput{}, fmt.Errorf("creating Open Match ticket: %w", err)
	}
	return nil, ticketOutput(created), nil
}

type OpenMatchTicketInput struct {
	TicketID string `json:"ticketId" jsonschema:"Open Match ticket ID"`
	Cluster  string `json:"cluster,omitempty" jsonschema:"Cluster to target; omit for the default cluster"`
}

func (s *server) openMatchTicketStatus(ctx context.Context, req *mcp.CallToolRequest, in OpenMatchTicketInput) (*mcp.CallToolResult, OpenMatchTicketOutput, error) {
	cl, err := s.c.get(in.Cluster)
	if err != nil {
		return nil, OpenMatchTicketOutput{}, err
	}
	if cl.omFrontend == nil {
		return nil, OpenMatchTicketOutput{}, errOpenMatchNotConfigured
	}
	t, err := cl.omFrontend.GetTicket(ctx, &pb.GetTicketRequest{TicketId: in.TicketID})
	if err != nil {
		return nil, OpenMatchTicketOutput{}, fmt.Errorf("getting Open Match ticket %q: %w", in.TicketID, err)
	}
	return nil, ticketOutput(t), nil
}

type OpenMatchCancelTicketOutput struct {
	TicketID string `json:"ticketId"`
	Deleted  bool   `json:"deleted"`
}

func (s *server) openMatchCancelTicket(ctx context.Context, req *mcp.CallToolRequest, in OpenMatchTicketInput) (*mcp.CallToolResult, OpenMatchCancelTicketOutput, error) {
	cl, err := s.c.get(in.Cluster)
	if err != nil {
		return nil, OpenMatchCancelTicketOutput{}, err
	}
	if cl.omFrontend == nil {
		return nil, OpenMatchCancelTicketOutput{}, errOpenMatchNotConfigured
	}
	if _, err := cl.omFrontend.DeleteTicket(ctx, &pb.DeleteTicketRequest{TicketId: in.TicketID}); err != nil {
		return nil, OpenMatchCancelTicketOutput{}, fmt.Errorf("canceling Open Match ticket %q: %w", in.TicketID, err)
	}
	return nil, OpenMatchCancelTicketOutput{TicketID: in.TicketID, Deleted: true}, nil
}
