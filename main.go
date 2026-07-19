package main

import (
	"context"
	"log"
	"runtime/debug"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type server struct {
	c *registry
}

// The module version go install stamped into the binary, so deployed copies
// self-report which release they actually are.
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func main() {
	reg, err := newRegistry()
	if err != nil {
		log.Fatalf("agones-mcp: %v", err)
	}
	s := &server{c: reg}

	srv := mcp.NewServer(&mcp.Implementation{Name: "agones-mcp", Version: version()}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_clusters",
		Description: "List configured clusters (only meaningful when AGONES_MCP_CLUSTERS is set) and which one is the default",
	}, s.listClusters)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_fleets",
		Description: "List Agones Fleets with desired/ready/allocated/reserved replica counts",
	}, s.listFleets)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_gameservers",
		Description: "List Agones GameServers, filterable by state (Ready, Allocated, Unhealthy, ...) or owning fleet",
	}, s.listGameServers)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "gameserver_events",
		Description: "Get Kubernetes events for a GameServer to diagnose failures and state transitions",
	}, s.gameServerEvents)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "gameserver_logs",
		Description: "Fetch container logs for a GameServer. Use previous=true for a server that already crashed or restarted",
	}, s.gameServerLogs)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_autoscalers",
		Description: "List FleetAutoscalers with policy, bounds, and whether scaling is currently limited",
	}, s.listAutoscalers)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_capacity",
		Description: "Capacity report across fleets: utilization, autoscaler headroom, and warnings for fleets at ceiling or with no Ready servers",
	}, s.fleetCapacity)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "create_fleet",
		Description: "Create a new Fleet with a single-container GameServer template",
	}, s.createFleet)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "delete_fleet",
		Description: "Delete a Fleet and all its GameServers. Refuses if any are Allocated (live matches) unless force=true. The check is best-effort: an allocation landing at the same instant can still be disrupted",
	}, s.deleteFleet)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "create_autoscaler",
		Description: "Create a Buffer-policy FleetAutoscaler that keeps a target number (or percentage) of Ready servers available on a Fleet",
	}, s.createAutoscaler)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "update_autoscaler",
		Description: "Update a Buffer-policy FleetAutoscaler's buffer size, minimum, or maximum replicas",
	}, s.updateAutoscaler)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "delete_autoscaler",
		Description: "Delete a FleetAutoscaler. The Fleet and its GameServers are unaffected; the fleet simply stops being auto-scaled",
	}, s.deleteAutoscaler)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "scale_fleet",
		Description: "Set a Fleet's replica count. Scale-down removes Ready servers first; Allocated servers with live matches are never disrupted",
	}, s.scaleFleet)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "allocate_gameserver",
		Description: "Allocate a Ready GameServer from a fleet for a match, returning its address and ports. Optionally filter by Counter/List state (e.g. available player slots) and apply Counter/List changes at allocation time",
	}, s.allocateGameServer)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "delete_gameserver",
		Description: "Delete a GameServer. Refuses to delete Allocated servers (live matches) unless force=true",
	}, s.deleteGameServer)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "update_fleet_image",
		Description: "Update a Fleet's container image to trigger a rolling update. Agones never disrupts Allocated servers with live matches; use rollout_status to track progress",
	}, s.updateFleetImage)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "update_fleet_resources",
		Description: "Update CPU/memory requests and limits on a Fleet's container, triggering Agones's own allocation-aware rolling update",
	}, s.updateFleetResources)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "update_fleet_health",
		Description: "Update a Fleet's health-check settings (disabled, periodSeconds, failureThreshold, initialDelaySeconds), triggering Agones's own allocation-aware rolling update",
	}, s.updateFleetHealth)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "rollout_status",
		Description: "Report rolling update progress for a Fleet: current vs previous GameServerSets, percent complete, and whether old-version servers still have live matches blocking full completion",
	}, s.rolloutStatus)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "open_match_create_ticket",
		Description: "Enter a player or party into Open Match matchmaking, returning a ticket ID. Requires Open Match connectivity to be configured (AGONES_MCP_OPEN_MATCH_FRONTEND)",
	}, s.openMatchCreateTicket)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "open_match_ticket_status",
		Description: "Check an Open Match ticket: still searching, or matched with a connection assignment",
	}, s.openMatchTicketStatus)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "open_match_cancel_ticket",
		Description: "Withdraw an Open Match ticket from matchmaking",
	}, s.openMatchCancelTicket)

	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("agones-mcp: %v", err)
	}
}
