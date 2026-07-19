package main

import (
	"context"
	"strings"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	agonesfake "agones.dev/agones/pkg/client/clientset/versioned/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
)

// Fleet.Status here says 2 ready, 0 allocated - stale on purpose - while the
// live GameServers say otherwise; fleet_capacity should report the latter.
func TestFleetCapacity_UsesLiveGameServerState(t *testing.T) {
	staleFleet := testFleet("simple-fleet", "default", 2, 2, 0, 0, 2)
	liveReady := testGameServer("simple-fleet-a", "default", "simple-fleet", agonesv1.GameServerStateReady)
	liveAllocated := testGameServer("simple-fleet-b", "default", "simple-fleet", agonesv1.GameServerStateAllocated)

	s := newTestServer(staleFleet, liveReady, liveAllocated)

	_, out, err := s.fleetCapacity(context.Background(), nil, NamespaceInput{})
	if err != nil {
		t.Fatalf("fleetCapacity: %v", err)
	}
	if len(out.Fleets) != 1 {
		t.Fatalf("expected 1 fleet, got %d", len(out.Fleets))
	}
	got := out.Fleets[0]
	if got.Allocated != 1 {
		t.Errorf("Allocated = %d, want 1 (must come from live GameServer state, not stale Fleet.Status)", got.Allocated)
	}
	if got.Ready != 1 {
		t.Errorf("Ready = %d, want 1", got.Ready)
	}
}

func TestFleetCapacity_WarnsWhenNoReadyServers(t *testing.T) {
	fleet := testFleet("empty-fleet", "default", 2, 0, 0, 0, 2)
	allocated := testGameServer("empty-fleet-a", "default", "empty-fleet", agonesv1.GameServerStateAllocated)
	allocated2 := testGameServer("empty-fleet-b", "default", "empty-fleet", agonesv1.GameServerStateAllocated)

	s := newTestServer(fleet, allocated, allocated2)
	_, out, err := s.fleetCapacity(context.Background(), nil, NamespaceInput{})
	if err != nil {
		t.Fatalf("fleetCapacity: %v", err)
	}
	if len(out.Fleets[0].Warnings) == 0 {
		t.Error("expected a warning when Ready count is 0 but the fleet has servers, got none")
	}
}

func TestFleetCapacity_WarnsAtAutoscalerCeiling(t *testing.T) {
	fleet := testFleet("capped-fleet", "default", 6, 6, 0, 0, 6)
	scaler := testAutoscaler("capped-fleet-as", "default", "capped-fleet", 2, 2, 6, true)
	s := newTestServer(fleet, scaler)

	_, out, err := s.fleetCapacity(context.Background(), nil, NamespaceInput{})
	if err != nil {
		t.Fatalf("fleetCapacity: %v", err)
	}
	if !out.Fleets[0].AtCeiling {
		t.Error("expected AtCeiling=true when autoscaler reports ScalingLimited")
	}
	if len(out.Fleets[0].Warnings) == 0 {
		t.Error("expected a warning when fleet is at its autoscaler ceiling")
	}
	if out.Fleets[0].AutoscalerCeiling != 6 {
		t.Errorf("AutoscalerCeiling = %d, want 6", out.Fleets[0].AutoscalerCeiling)
	}
}

// Agones sets ScalingLimited when clamped at EITHER bound; an idle fleet
// parked at its minReplicas floor must not read as "at ceiling".
func TestFleetCapacity_FloorClampIsNotAtCeiling(t *testing.T) {
	fleet := testFleet("idle-fleet", "default", 5, 5, 0, 0, 5)
	scaler := testAutoscaler("idle-fleet-as", "default", "idle-fleet", 2, 5, 20, false)
	scaler.Status.ScalingLimited = true
	scaler.Status.DesiredReplicas = 5 // pinned at the floor, far below max=20
	s := newTestServer(fleet, scaler)

	_, out, err := s.fleetCapacity(context.Background(), nil, NamespaceInput{})
	if err != nil {
		t.Fatalf("fleetCapacity: %v", err)
	}
	if out.Fleets[0].AtCeiling {
		t.Fatal("AtCeiling = true for a floor-clamped autoscaler; a fleet with maximal headroom must not report a capacity emergency")
	}
	for _, w := range out.Fleets[0].Warnings {
		if strings.Contains(w, "max replicas") {
			t.Fatalf("unexpected at-ceiling warning on a floor-clamped fleet: %v", out.Fleets[0].Warnings)
		}
	}
}

// Servers still booting must not deflate utilization: 2 allocated + 2 ready +
// 4 Scheduled is 50% of playable capacity in use, not 25%.
func TestFleetCapacity_UtilizationIgnoresBootingServers(t *testing.T) {
	fleet := testFleet("busy-fleet", "default", 8, 2, 2, 0, 8)
	objs := []runtime.Object{fleet,
		testGameServer("a", "default", "busy-fleet", agonesv1.GameServerStateAllocated),
		testGameServer("b", "default", "busy-fleet", agonesv1.GameServerStateAllocated),
		testGameServer("c", "default", "busy-fleet", agonesv1.GameServerStateReady),
		testGameServer("d", "default", "busy-fleet", agonesv1.GameServerStateReady),
		testGameServer("e", "default", "busy-fleet", agonesv1.GameServerStateScheduled),
		testGameServer("f", "default", "busy-fleet", agonesv1.GameServerStateScheduled),
		testGameServer("g", "default", "busy-fleet", agonesv1.GameServerStateScheduled),
		testGameServer("h", "default", "busy-fleet", agonesv1.GameServerStateScheduled),
	}
	s := newTestServer(objs...)

	_, out, err := s.fleetCapacity(context.Background(), nil, NamespaceInput{})
	if err != nil {
		t.Fatalf("fleetCapacity: %v", err)
	}
	if got := out.Fleets[0].Utilization; got != 50 {
		t.Fatalf("Utilization = %v, want 50 (allocated / (allocated+ready))", got)
	}
}

// The worst outage: desired replicas but zero live GameServers (fleet-wide
// image-pull failure). Must still warn.
func TestFleetCapacity_WarnsWhenDesiredButNoLiveServers(t *testing.T) {
	fleet := testFleet("dead-fleet", "default", 3, 0, 0, 0, 0)
	s := newTestServer(fleet)

	_, out, err := s.fleetCapacity(context.Background(), nil, NamespaceInput{})
	if err != nil {
		t.Fatalf("fleetCapacity: %v", err)
	}
	if len(out.Fleets[0].Warnings) == 0 {
		t.Fatal("expected a no-Ready-servers warning for a fleet with desired replicas and zero live GameServers")
	}
}

func TestListGameServers_RejectsUnknownStateFilter(t *testing.T) {
	s := newTestServer()
	_, _, err := s.listGameServers(context.Background(), nil, GameServerListInput{State: "Crashed"})
	if err == nil {
		t.Fatal("expected an error for an unknown state filter; silently returning nothing reads as a false all-clear")
	}
	if !strings.Contains(err.Error(), "Allocated") {
		t.Fatalf("expected the error to list valid states, got: %v", err)
	}
}

func TestListGameServers_RejectsNegativeLimit(t *testing.T) {
	s := newTestServer()
	_, _, err := s.listGameServers(context.Background(), nil, GameServerListInput{Limit: -1})
	if err == nil {
		t.Fatal("expected an error for a negative limit, got nil")
	}
}

func TestListGameServers_FiltersByStateAndFleet(t *testing.T) {
	readyA := testGameServer("a", "default", "fleet-x", agonesv1.GameServerStateReady)
	allocatedB := testGameServer("b", "default", "fleet-x", agonesv1.GameServerStateAllocated)
	readyC := testGameServer("c", "default", "fleet-y", agonesv1.GameServerStateReady)

	s := newTestServer(readyA, allocatedB, readyC)

	_, out, err := s.listGameServers(context.Background(), nil, GameServerListInput{State: "Ready"})
	if err != nil {
		t.Fatalf("listGameServers: %v", err)
	}
	if out.Count != 2 {
		t.Errorf("state filter: count = %d, want 2", out.Count)
	}

	_, out, err = s.listGameServers(context.Background(), nil, GameServerListInput{Fleet: "fleet-x"})
	if err != nil {
		t.Fatalf("listGameServers: %v", err)
	}
	if out.Count != 2 {
		t.Errorf("fleet filter: count = %d, want 2", out.Count)
	}

	_, out, err = s.listGameServers(context.Background(), nil, GameServerListInput{State: "Allocated", Fleet: "fleet-x"})
	if err != nil {
		t.Fatalf("listGameServers: %v", err)
	}
	if out.Count != 1 || out.GameServers[0].Name != "b" {
		t.Errorf("combined filter: got %+v, want exactly [b]", out.GameServers)
	}
}

func TestListGameServers_SurfacesCountersAndLists(t *testing.T) {
	gs := testGameServerWithCounters("a", "default", "fleet-x", agonesv1.GameServerStateReady,
		map[string]agonesv1.CounterStatus{"players": {Count: 2, Capacity: 10}},
		map[string]agonesv1.ListStatus{"sessions": {Capacity: 5, Values: []string{"s1"}}})
	s := newTestServer(gs)

	_, out, err := s.listGameServers(context.Background(), nil, GameServerListInput{})
	if err != nil {
		t.Fatalf("listGameServers: %v", err)
	}
	if out.Count != 1 {
		t.Fatalf("Count = %d, want 1", out.Count)
	}
	got := out.GameServers[0]
	if got.Counters["players"].Count != 2 || got.Counters["players"].Capacity != 10 {
		t.Errorf("Counters[players] = %+v, want {2 10}", got.Counters["players"])
	}
	if len(got.Lists["sessions"].Values) != 1 || got.Lists["sessions"].Values[0] != "s1" {
		t.Errorf("Lists[sessions] = %+v, want Values=[s1]", got.Lists["sessions"])
	}
}

func TestListGameServers_StateFilterIsCaseInsensitive(t *testing.T) {
	ready := testGameServer("a", "default", "fleet-x", agonesv1.GameServerStateReady)
	s := newTestServer(ready)

	_, out, err := s.listGameServers(context.Background(), nil, GameServerListInput{State: "ready"})
	if err != nil {
		t.Fatalf("listGameServers: %v", err)
	}
	if out.Count != 1 {
		t.Errorf("lowercase state filter: count = %d, want 1 (a typo'd case shouldn't silently return nothing)", out.Count)
	}
}

// Note: the fake enforces LabelSelector itself, so this only checks normal
// filtering behavior, not the re-filter's defense against a server that
// ignores the selector - that part isn't unit-testable against this fake.
func TestListGameServers_FleetFilterExcludesOtherFleets(t *testing.T) {
	inFleet := testGameServer("in-fleet", "default", "fleet-x", agonesv1.GameServerStateReady)
	otherFleet := testGameServer("other-fleet", "default", "fleet-y", agonesv1.GameServerStateReady)
	s := newTestServer(inFleet, otherFleet)

	_, out, err := s.listGameServers(context.Background(), nil, GameServerListInput{Fleet: "fleet-x"})
	if err != nil {
		t.Fatalf("listGameServers: %v", err)
	}
	if out.Count != 1 || out.GameServers[0].Name != "in-fleet" {
		t.Fatalf("expected only fleet-x's GameServer, got %+v", out.GameServers)
	}
}

func TestListGameServers_EmptyClusterReturnsEmptyNotNil(t *testing.T) {
	s := newTestServer()
	_, out, err := s.listGameServers(context.Background(), nil, GameServerListInput{})
	if err != nil {
		t.Fatalf("listGameServers: %v", err)
	}
	if out.GameServers == nil {
		t.Error("GameServers slice is nil, want an empty slice so callers get [] not null in JSON")
	}
	if out.Count != 0 {
		t.Errorf("Count = %d, want 0", out.Count)
	}
}

// Reactor always returns a non-empty Continue token, standing in for a
// server that never stops paginating.
func TestListAllGameServers_AbortsRatherThanLoopingForeverOnRunawayContinue(t *testing.T) {
	s := newTestServer()
	ag, ok := testClients(s).agones.(*agonesfake.Clientset)
	if !ok {
		t.Fatal("expected the fake Agones clientset")
	}
	calls := 0
	ag.PrependReactor("list", "gameservers", func(action ktesting.Action) (bool, runtime.Object, error) {
		calls++
		return true, &agonesv1.GameServerList{
			ListMeta: metav1.ListMeta{Continue: "always-more"},
		}, nil
	})

	_, err := listAllGameServers(context.Background(), testClients(s), "default", "")
	if err == nil {
		t.Fatal("expected an error once the page cap was exceeded, got nil")
	}
	if !strings.Contains(err.Error(), "pages") {
		t.Fatalf("expected the error to explain the page-cap abort, got: %v", err)
	}
	if calls != listAllGameServersMaxPages {
		t.Fatalf("expected exactly %d calls (the cap), got %d", listAllGameServersMaxPages, calls)
	}
}
