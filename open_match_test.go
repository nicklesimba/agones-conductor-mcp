package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
	"open-match.dev/open-match/pkg/pb"
)

// fakeFrontend is a minimal in-memory Open Match Frontend, backed by a real
// gRPC server over an in-process bufconn listener instead of a hand-rolled
// interface substitute. Dialing it exercises the actual generated client
// code - marshaling, the real FrontendServiceClient interface - just
// without a cluster on the other end.
type fakeFrontend struct {
	pb.UnimplementedFrontendServiceServer
	mu      sync.Mutex
	nextID  int
	tickets map[string]*pb.Ticket
}

func (f *fakeFrontend) CreateTicket(ctx context.Context, req *pb.CreateTicketRequest) (*pb.Ticket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("ticket-%d", f.nextID)
	t := req.GetTicket()
	t.Id = id
	f.tickets[id] = t
	return t, nil
}

func (f *fakeFrontend) GetTicket(ctx context.Context, req *pb.GetTicketRequest) (*pb.Ticket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tickets[req.GetTicketId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "ticket %q not found", req.GetTicketId())
	}
	return t, nil
}

func (f *fakeFrontend) DeleteTicket(ctx context.Context, req *pb.DeleteTicketRequest) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tickets, req.GetTicketId())
	return &emptypb.Empty{}, nil
}

// assign simulates the director finalizing a match for a ticket.
func (f *fakeFrontend) assign(ticketID, connection string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tickets[ticketID]; ok {
		t.Assignment = &pb.Assignment{Connection: connection}
	}
}

func newOpenMatchTestServer(t *testing.T) (*server, *fakeFrontend) {
	t.Helper()
	fe := &fakeFrontend{tickets: map[string]*pb.Ticket{}}

	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	pb.RegisterFrontendServiceServer(grpcServer, fe)
	go func() {
		_ = grpcServer.Serve(lis)
	}()
	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dialing bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	c := &clients{omFrontend: pb.NewFrontendServiceClient(conn)}
	return &server{c: &registry{def: "test", byName: map[string]*clients{"test": c}}}, fe
}

func TestOpenMatchCreateTicket_ReturnsTicketWithTags(t *testing.T) {
	s, _ := newOpenMatchTestServer(t)

	_, out, err := s.openMatchCreateTicket(context.Background(), nil, OpenMatchCreateTicketInput{
		Tags: []string{"mode.demo"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.TicketID == "" {
		t.Fatal("expected a non-empty ticket ID")
	}
	if len(out.Tags) != 1 || out.Tags[0] != "mode.demo" {
		t.Fatalf("expected tags [mode.demo], got %v", out.Tags)
	}
	if out.Assigned {
		t.Fatal("a freshly created ticket should not be Assigned")
	}
}

func TestOpenMatchCreateTicket_RejectsTooManyTags(t *testing.T) {
	s, _ := newOpenMatchTestServer(t)
	tags := make([]string, maxTicketTags+1)
	for i := range tags {
		tags[i] = "tag"
	}

	_, _, err := s.openMatchCreateTicket(context.Background(), nil, OpenMatchCreateTicketInput{Tags: tags})
	if err == nil {
		t.Fatal("expected an error for exceeding the tag count limit, got nil")
	}
}

func TestOpenMatchCreateTicket_RejectsOversizedField(t *testing.T) {
	s, _ := newOpenMatchTestServer(t)

	_, _, err := s.openMatchCreateTicket(context.Background(), nil, OpenMatchCreateTicketInput{
		Tags: []string{strings.Repeat("x", maxTicketFieldStringLen+1)},
	})
	if err == nil {
		t.Fatal("expected an error for an oversized tag, got nil")
	}
}

func TestOpenMatchCreateTicket_RejectsTooManyStringArgs(t *testing.T) {
	s, _ := newOpenMatchTestServer(t)
	args := make(map[string]string, maxTicketSearchEntries+1)
	for i := 0; i < maxTicketSearchEntries+1; i++ {
		args[fmt.Sprintf("key-%d", i)] = "v"
	}

	_, _, err := s.openMatchCreateTicket(context.Background(), nil, OpenMatchCreateTicketInput{StringArgs: args})
	if err == nil {
		t.Fatal("expected an error for exceeding the stringArgs entry limit, got nil")
	}
}

func TestOpenMatchTicketStatus_ReportsAssignmentWhenPresent(t *testing.T) {
	s, fe := newOpenMatchTestServer(t)
	_, created, err := s.openMatchCreateTicket(context.Background(), nil, OpenMatchCreateTicketInput{Tags: []string{"mode.demo"}})
	if err != nil {
		t.Fatalf("unexpected error creating ticket: %v", err)
	}

	_, before, err := s.openMatchTicketStatus(context.Background(), nil, OpenMatchTicketInput{TicketID: created.TicketID})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if before.Assigned {
		t.Fatal("expected not yet Assigned before any match/assignment happened")
	}

	fe.assign(created.TicketID, "10.0.0.9:7777")
	_, after, err := s.openMatchTicketStatus(context.Background(), nil, OpenMatchTicketInput{TicketID: created.TicketID})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !after.Assigned || after.Connection != "10.0.0.9:7777" {
		t.Fatalf("expected Assigned=true with the connection surfaced, got %+v", after)
	}
}

func TestOpenMatchTicketStatus_UnknownTicketReturnsError(t *testing.T) {
	s, _ := newOpenMatchTestServer(t)

	_, _, err := s.openMatchTicketStatus(context.Background(), nil, OpenMatchTicketInput{TicketID: "does-not-exist"})
	if err == nil {
		t.Fatal("expected error for unknown ticket, got nil")
	}
}

func TestOpenMatchCancelTicket_DeletesAndSubsequentGetFails(t *testing.T) {
	s, _ := newOpenMatchTestServer(t)
	_, created, err := s.openMatchCreateTicket(context.Background(), nil, OpenMatchCreateTicketInput{Tags: []string{"mode.demo"}})
	if err != nil {
		t.Fatalf("unexpected error creating ticket: %v", err)
	}

	_, cancelOut, err := s.openMatchCancelTicket(context.Background(), nil, OpenMatchTicketInput{TicketID: created.TicketID})
	if err != nil {
		t.Fatalf("unexpected error canceling: %v", err)
	}
	if !cancelOut.Deleted {
		t.Fatal("expected Deleted = true")
	}

	if _, _, err := s.openMatchTicketStatus(context.Background(), nil, OpenMatchTicketInput{TicketID: created.TicketID}); err == nil {
		t.Fatal("expected GetTicket to fail after cancellation, got nil error")
	}
}

func TestOpenMatchTools_NotConfiguredReturnsClearError(t *testing.T) {
	s := newTestServer()

	if _, _, err := s.openMatchCreateTicket(context.Background(), nil, OpenMatchCreateTicketInput{Tags: []string{"mode.demo"}}); err != errOpenMatchNotConfigured {
		t.Fatalf("expected errOpenMatchNotConfigured, got %v", err)
	}
	if _, _, err := s.openMatchTicketStatus(context.Background(), nil, OpenMatchTicketInput{TicketID: "x"}); err != errOpenMatchNotConfigured {
		t.Fatalf("expected errOpenMatchNotConfigured, got %v", err)
	}
	if _, _, err := s.openMatchCancelTicket(context.Background(), nil, OpenMatchTicketInput{TicketID: "x"}); err != errOpenMatchNotConfigured {
		t.Fatalf("expected errOpenMatchNotConfigured, got %v", err)
	}
}
