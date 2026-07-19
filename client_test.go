package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	agonesfake "agones.dev/agones/pkg/client/clientset/versioned/fake"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"open-match.dev/open-match/pkg/pb"
)

func TestRegistry_GetReturnsDefaultWhenClusterEmpty(t *testing.T) {
	def := &clients{agones: agonesfake.NewSimpleClientset(), core: k8sfake.NewSimpleClientset()}
	r := &registry{def: "us-west", byName: map[string]*clients{"us-west": def}}

	got, err := r.get("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != def {
		t.Fatal("get(\"\") did not return the default cluster's clients")
	}
}

func TestRegistry_GetReturnsNamedCluster(t *testing.T) {
	west := &clients{agones: agonesfake.NewSimpleClientset(), core: k8sfake.NewSimpleClientset()}
	east := &clients{agones: agonesfake.NewSimpleClientset(), core: k8sfake.NewSimpleClientset()}
	r := &registry{def: "us-west", byName: map[string]*clients{"us-west": west, "us-east": east}}

	got, err := r.get("us-east")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != east {
		t.Fatal("get(\"us-east\") did not return the us-east cluster's clients")
	}
}

func TestRegistry_GetUnknownClusterReturnsHelpfulError(t *testing.T) {
	r := &registry{def: "us-west", byName: map[string]*clients{"us-west": {}, "us-east": {}}}

	_, err := r.get("eu-central")
	if err == nil {
		t.Fatal("expected error for unknown cluster name, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "eu-central") || !strings.Contains(msg, "us-west") || !strings.Contains(msg, "us-east") {
		t.Fatalf("expected error to name the bad cluster and list known ones, got: %q", msg)
	}
}

func TestListGameServers_RoutesToNamedCluster(t *testing.T) {
	west := agonesfake.NewSimpleClientset(testGameServer("west-gs", "default", "simple-fleet", agonesv1.GameServerStateReady))
	east := agonesfake.NewSimpleClientset(testGameServer("east-gs", "default", "simple-fleet", agonesv1.GameServerStateReady))
	s := &server{c: &registry{
		def: "us-west",
		byName: map[string]*clients{
			"us-west": {agones: west, core: k8sfake.NewSimpleClientset()},
			"us-east": {agones: east, core: k8sfake.NewSimpleClientset()},
		},
	}}

	_, out, err := s.listGameServers(context.Background(), nil, GameServerListInput{Namespace: "default", Cluster: "us-east"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Count != 1 || out.GameServers[0].Name != "east-gs" {
		t.Fatalf("expected cluster=us-east to route to the east clientset (east-gs), got %+v", out.GameServers)
	}

	_, out, err = s.listGameServers(context.Background(), nil, GameServerListInput{Namespace: "default"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Count != 1 || out.GameServers[0].Name != "west-gs" {
		t.Fatalf("expected omitted cluster to route to the default (west-gs), got %+v", out.GameServers)
	}
}

func TestListClusters_ReportsNamesAndDefault(t *testing.T) {
	s := &server{c: &registry{
		def: "us-west",
		byName: map[string]*clients{
			"us-west": {agones: agonesfake.NewSimpleClientset(), core: k8sfake.NewSimpleClientset()},
			"us-east": {agones: agonesfake.NewSimpleClientset(), core: k8sfake.NewSimpleClientset()},
		},
	}}

	_, out, err := s.listClusters(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Default != "us-west" {
		t.Errorf("Default = %q, want us-west", out.Default)
	}
	if len(out.Clusters) != 2 || out.Clusters[0] != "us-east" || out.Clusters[1] != "us-west" {
		t.Fatalf("expected sorted [us-east us-west], got %v", out.Clusters)
	}
}

func TestParseClusterNames_SplitsTrimsAndDropsEmpty(t *testing.T) {
	got := parseClusterNames(" us-west ,us-east,, eu-central")
	want := []string{"us-west", "us-east", "eu-central"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestParseClusterNames_DeduplicatesPreservingFirstOccurrence(t *testing.T) {
	got := parseClusterNames("us-west,us-east,us-west,us-east,eu-central")
	want := []string{"us-west", "us-east", "eu-central"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestResolveDefaultCluster_FallsBackToFirstWhenContextUnset(t *testing.T) {
	def, err := resolveDefaultCluster([]string{"us-west", "us-east"}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if def != "us-west" {
		t.Errorf("def = %q, want us-west", def)
	}
}

func TestResolveDefaultCluster_UsesNamedContext(t *testing.T) {
	def, err := resolveDefaultCluster([]string{"us-west", "us-east"}, "us-east")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if def != "us-east" {
		t.Errorf("def = %q, want us-east", def)
	}
}

func TestResolveDefaultCluster_ErrorsWhenContextNotInList(t *testing.T) {
	_, err := resolveDefaultCluster([]string{"us-west", "us-east"}, "eu-central")
	if err == nil {
		t.Fatal("expected an error when AGONES_MCP_CONTEXT names a cluster absent from AGONES_MCP_CLUSTERS, got nil")
	}
	if !strings.Contains(err.Error(), "eu-central") || !strings.Contains(err.Error(), "us-west") {
		t.Fatalf("expected error to name the bad context and the known clusters, got: %q", err.Error())
	}
}

func TestParseOpenMatchFrontends_ParsesValidPairs(t *testing.T) {
	got, err := parseOpenMatchFrontends("us-west=host1:50504, us-east=host2:50504")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["us-west"] != "host1:50504" || got["us-east"] != "host2:50504" {
		t.Fatalf("got %v", got)
	}
}

func TestParseOpenMatchFrontends_EmptyStringReturnsEmptyMap(t *testing.T) {
	got, err := parseOpenMatchFrontends("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %v", got)
	}
}

func TestParseOpenMatchFrontends_MalformedEntryErrors(t *testing.T) {
	if _, err := parseOpenMatchFrontends("us-west-no-equals-sign"); err == nil {
		t.Fatal("expected an error for a malformed entry, got nil")
	}
	if _, err := parseOpenMatchFrontends("=host:50504"); err == nil {
		t.Fatal("expected an error for an entry with an empty cluster name, got nil")
	}
	if _, err := parseOpenMatchFrontends("us-west="); err == nil {
		t.Fatal("expected an error for an entry with an empty address, got nil")
	}
}

func TestValidateOpenMatchFrontends_OKWhenAllKnown(t *testing.T) {
	err := validateOpenMatchFrontends(map[string]string{"us-west": "host:50504"}, []string{"us-west", "us-east"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateOpenMatchFrontends_ErrorsOnUnknownCluster(t *testing.T) {
	err := validateOpenMatchFrontends(map[string]string{"eu-central": "host:50504"}, []string{"us-west", "us-east"})
	if err == nil {
		t.Fatal("expected an error when a frontend names a cluster absent from AGONES_MCP_CLUSTERS, got nil")
	}
	if !strings.Contains(err.Error(), "eu-central") {
		t.Fatalf("expected error to name the bad cluster, got: %q", err.Error())
	}
}

type slowFrontend struct {
	pb.UnimplementedFrontendServiceServer
	delay time.Duration
}

func (f *slowFrontend) CreateTicket(ctx context.Context, req *pb.CreateTicketRequest) (*pb.Ticket, error) {
	select {
	case <-time.After(f.delay):
		return req.GetTicket(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Server sleeps far longer than the interceptor's timeout.
func TestTimeoutUnaryInterceptor_AbortsHungCall(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	pb.RegisterFrontendServiceServer(grpcServer, &slowFrontend{delay: 2 * time.Second})
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	const testTimeout = 100 * time.Millisecond
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(timeoutUnaryInterceptor(testTimeout)),
	)
	if err != nil {
		t.Fatalf("dialing bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client := pb.NewFrontendServiceClient(conn)
	start := time.Now()
	_, err = client.CreateTicket(context.Background(), &pb.CreateTicketRequest{Ticket: &pb.Ticket{}})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the call to fail once the interceptor's timeout elapsed, got nil error")
	}
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("call took %v - the interceptor did not bound it, it waited for the slow server", elapsed)
	}
}

func TestLoadKubeConfig_SetsTimeout(t *testing.T) {
	dir := t.TempDir()
	kubeconfigPath := filepath.Join(dir, "config")
	const kubeconfigYAML = `
apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user: {}
`
	if err := os.WriteFile(kubeconfigPath, []byte(kubeconfigYAML), 0o600); err != nil {
		t.Fatalf("writing temp kubeconfig: %v", err)
	}
	t.Setenv("KUBECONFIG", kubeconfigPath)

	cfg, err := loadKubeConfig("test")
	if err != nil {
		t.Fatalf("loadKubeConfig: %v", err)
	}
	if cfg.Timeout != defaultRequestTimeout {
		t.Fatalf("Timeout = %v, want %v", cfg.Timeout, defaultRequestTimeout)
	}
}

// httptest.Server.Close() blocks until the handler returns, so this test's
// own reported duration includes the full handler delay even though the
// client call itself aborts almost immediately - check elapsed, not that.
func TestRestConfigTimeout_AbortsHungCall(t *testing.T) {
	const handlerDelay = 500 * time.Millisecond
	const clientTimeout = 50 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(handlerDelay)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &rest.Config{Host: srv.URL, Timeout: clientTimeout}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("building client: %v", err)
	}

	start := time.Now()
	_, err = client.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error once the timeout elapsed, got nil")
	}
	// The exact wrapping varies by platform/timing; either form proves the
	// client bounded the call.
	if !strings.Contains(err.Error(), "Client.Timeout") && !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("expected a timeout error, got: %v", err)
	}
	if elapsed >= handlerDelay {
		t.Fatalf("call took %v (>= the %v handler delay) - rest.Config.Timeout did not bound it, it waited for the slow server", elapsed, handlerDelay)
	}
}
