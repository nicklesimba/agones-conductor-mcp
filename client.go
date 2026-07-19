package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	versioned "agones.dev/agones/pkg/client/clientset/versioned"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"open-match.dev/open-match/pkg/pb"
)

// No tool here uses Watch, so a flat timeout on every request is safe.
const defaultRequestTimeout = 30 * time.Second

// Interface types, not concrete *Clientset, so tests can swap in fakes.
// omFrontend is nil until Open Match is configured for this cluster.
type clients struct {
	agones     versioned.Interface
	core       kubernetes.Interface
	omFrontend pb.FrontendServiceClient
}

// One entry per configured cluster, keyed by context name.
type registry struct {
	def    string
	byName map[string]*clients
}

func (r *registry) get(name string) (*clients, error) {
	if name == "" {
		name = r.def
	}
	c, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("unknown cluster %q; known clusters: %s", name, strings.Join(r.names(), ", "))
	}
	return c, nil
}

func (r *registry) names() []string {
	names := make([]string, 0, len(r.byName))
	for n := range r.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// AGONES_MCP_CLUSTERS (comma-separated context names) opts into
// multi-cluster mode; otherwise this builds a single cluster from
// AGONES_MCP_CONTEXT or in-cluster config. Open Match is separately opt-in
// per cluster via AGONES_MCP_OPEN_MATCH_FRONTEND(S).
func newRegistry() (*registry, error) {
	if raw := os.Getenv("AGONES_MCP_CLUSTERS"); raw != "" {
		names := parseClusterNames(raw)
		if len(names) == 0 {
			return nil, fmt.Errorf("AGONES_MCP_CLUSTERS is set but contains no cluster names")
		}
		// The singular/plural frontend vars belong to different modes; a set
		// -but-ignored one means the user's Open Match config silently
		// vanished, which deserves the same loud failure as a typo'd context.
		if os.Getenv("AGONES_MCP_OPEN_MATCH_FRONTEND") != "" {
			return nil, fmt.Errorf("AGONES_MCP_OPEN_MATCH_FRONTEND is ignored in multi-cluster mode; use AGONES_MCP_OPEN_MATCH_FRONTENDS (clusterName=host:port pairs)")
		}
		omFrontends, err := parseOpenMatchFrontends(os.Getenv("AGONES_MCP_OPEN_MATCH_FRONTENDS"))
		if err != nil {
			return nil, err
		}
		if err := validateOpenMatchFrontends(omFrontends, names); err != nil {
			return nil, err
		}
		def, err := resolveDefaultCluster(names, os.Getenv("AGONES_MCP_CONTEXT"))
		if err != nil {
			return nil, err
		}
		byName := map[string]*clients{}
		for _, n := range names {
			cfg, err := loadKubeConfig(n)
			if err != nil {
				return nil, fmt.Errorf("loading kubeconfig for cluster %q: %w", n, err)
			}
			c, err := newClients(cfg, omFrontends[n])
			if err != nil {
				return nil, fmt.Errorf("building clients for cluster %q: %w", n, err)
			}
			byName[n] = c
		}
		return &registry{def: def, byName: byName}, nil
	}

	if os.Getenv("AGONES_MCP_OPEN_MATCH_FRONTENDS") != "" {
		return nil, fmt.Errorf("AGONES_MCP_OPEN_MATCH_FRONTENDS is ignored in single-cluster mode; use AGONES_MCP_OPEN_MATCH_FRONTEND (host:port), or set AGONES_MCP_CLUSTERS for multi-cluster mode")
	}
	cfg, err := loadKubeConfig(os.Getenv("AGONES_MCP_CONTEXT"))
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	c, err := newClients(cfg, os.Getenv("AGONES_MCP_OPEN_MATCH_FRONTEND"))
	if err != nil {
		return nil, err
	}
	const def = "default"
	return &registry{def: def, byName: map[string]*clients{def: c}}, nil
}

// Splits, trims, and dedupes while preserving order (the first name is the
// fallback default).
func parseClusterNames(raw string) []string {
	var names []string
	seen := map[string]bool{}
	for _, n := range strings.Split(raw, ",") {
		if n = strings.TrimSpace(n); n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	return names
}

// An AGONES_MCP_CONTEXT that doesn't match any configured cluster is
// probably a typo, so this errors instead of silently falling back.
func resolveDefaultCluster(names []string, contextEnv string) (string, error) {
	if contextEnv == "" {
		return names[0], nil
	}
	for _, n := range names {
		if n == contextEnv {
			return contextEnv, nil
		}
	}
	return "", fmt.Errorf("AGONES_MCP_CONTEXT %q is not one of the configured clusters in AGONES_MCP_CLUSTERS: %s", contextEnv, strings.Join(names, ", "))
}

func validateOpenMatchFrontends(frontends map[string]string, names []string) error {
	known := map[string]bool{}
	for _, n := range names {
		known[n] = true
	}
	for n := range frontends {
		if !known[n] {
			return fmt.Errorf("AGONES_MCP_OPEN_MATCH_FRONTENDS names cluster %q, which is not in AGONES_MCP_CLUSTERS: %s", n, strings.Join(names, ", "))
		}
	}
	return nil
}

func parseOpenMatchFrontends(raw string) (map[string]string, error) {
	frontends := map[string]string{}
	if raw == "" {
		return frontends, nil
	}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, addr, ok := strings.Cut(pair, "=")
		if !ok || name == "" || addr == "" {
			return nil, fmt.Errorf("invalid AGONES_MCP_OPEN_MATCH_FRONTENDS entry %q; want clusterName=host:port", pair)
		}
		frontends[name] = addr
	}
	return frontends, nil
}

func newClients(cfg *rest.Config, omFrontendAddr string) (*clients, error) {
	ag, err := versioned.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building agones client: %w", err)
	}
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building core client: %w", err)
	}
	c := &clients{agones: ag, core: core}
	if omFrontendAddr != "" {
		fe, err := dialOpenMatchFrontend(omFrontendAddr)
		if err != nil {
			return nil, fmt.Errorf("connecting to Open Match frontend at %q: %w", omFrontendAddr, err)
		}
		c.omFrontend = fe
	}
	return c, nil
}

// addr must already be reachable; this doesn't tunnel to it.
func dialOpenMatchFrontend(addr string) (pb.FrontendServiceClient, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(timeoutUnaryInterceptor(defaultRequestTimeout)),
	)
	if err != nil {
		return nil, err
	}
	return pb.NewFrontendServiceClient(conn), nil
}

func timeoutUnaryInterceptor(timeout time.Duration) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func loadKubeConfig(context string) (*rest.Config, error) {
	cfg, err := resolveKubeConfig(context)
	if err != nil {
		return nil, err
	}
	cfg.Timeout = defaultRequestTimeout
	return cfg, nil
}

func resolveKubeConfig(context string) (*rest.Config, error) {
	if context == "" {
		cfg, err := rest.InClusterConfig()
		if err == nil {
			return cfg, nil
		}
		// Genuinely running in a Pod but the config is broken: surface that
		// error instead of a misleading kubeconfig one.
		if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
			return nil, fmt.Errorf("in-cluster config: %w", err)
		}
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	if context != "" {
		overrides.CurrentContext = context
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
}
