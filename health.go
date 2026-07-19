package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type AgonesHealthInput struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"Namespace Agones is installed in; defaults to agones-system"`
	Cluster   string `json:"cluster,omitempty" jsonschema:"Cluster to target; omit for the default cluster"`
}

type ComponentHealth struct {
	Component string `json:"component"`
	Ready     int    `json:"ready"`
	Total     int    `json:"total"`
	Restarts  int32  `json:"restarts" jsonschema:"Total container restarts across the component's pods; a climbing number means crash-looping"`
}

type AgonesHealthOutput struct {
	Healthy    bool              `json:"healthy" jsonschema:"True when every Agones component has at least one ready pod"`
	Components []ComponentHealth `json:"components"`
	Warnings   []string          `json:"warnings,omitempty"`
}

// The 3am question no fleet tool answers: is Agones itself broken? Pods are
// grouped by their agones.dev/role label (controller, allocator, extensions,
// ping).
func (s *server) agonesHealth(ctx context.Context, req *mcp.CallToolRequest, in AgonesHealthInput) (*mcp.CallToolResult, AgonesHealthOutput, error) {
	cl, err := s.c.get(in.Cluster)
	if err != nil {
		return nil, AgonesHealthOutput{}, err
	}
	namespace := in.Namespace
	if namespace == "" {
		namespace = "agones-system"
	}
	pods, err := cl.core.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=agones",
	})
	if err != nil {
		return nil, AgonesHealthOutput{}, fmt.Errorf("listing Agones pods in %q: %w", namespace, err)
	}

	byRole := map[string]*ComponentHealth{}
	for _, pod := range pods.Items {
		role := pod.Labels["agones.dev/role"]
		if role == "" {
			role = "unknown"
		}
		c, ok := byRole[role]
		if !ok {
			c = &ComponentHealth{Component: role}
			byRole[role] = c
		}
		c.Total++
		if podReady(&pod) {
			c.Ready++
		}
		for _, cs := range pod.Status.ContainerStatuses {
			c.Restarts += cs.RestartCount
		}
	}

	out := AgonesHealthOutput{Healthy: true, Components: []ComponentHealth{}}
	if len(byRole) == 0 {
		out.Healthy = false
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"no Agones pods found in namespace %q; is Agones installed there? (set namespace if it lives elsewhere)", namespace))
		return nil, out, nil
	}
	roles := make([]string, 0, len(byRole))
	for r := range byRole {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	for _, r := range roles {
		c := byRole[r]
		out.Components = append(out.Components, *c)
		if c.Ready == 0 {
			out.Healthy = false
			out.Warnings = append(out.Warnings, fmt.Sprintf(
				"%s: 0 of %d pods ready; Agones %s functionality is down", r, c.Total, r))
		}
	}
	if _, ok := byRole["controller"]; !ok {
		out.Healthy = false
		out.Warnings = append(out.Warnings, "no controller pods found; fleet scaling and GameServer lifecycle are not being reconciled")
	}
	return nil, out, nil
}

func podReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}
