package k8s

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestTargetFromGetenv(t *testing.T) {
	tests := []struct {
		name        string
		env         map[string]string
		wantNS      string
		wantContext string
	}{
		{name: "defaults", env: nil, wantNS: "gc", wantContext: ""},
		{name: "empty namespace falls back", env: map[string]string{"GC_K8S_NAMESPACE": ""}, wantNS: "gc"},
		{name: "namespace only", env: map[string]string{"GC_K8S_NAMESPACE": "agents"}, wantNS: "agents"},
		{
			name:        "namespace and context",
			env:         map[string]string{"GC_K8S_NAMESPACE": "agents", "GC_K8S_CONTEXT": "staging"},
			wantNS:      "agents",
			wantContext: "staging",
		},
		{name: "context only", env: map[string]string{"GC_K8S_CONTEXT": "staging"}, wantNS: "gc", wantContext: "staging"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns, kctx := targetFrom(func(key string) string { return tt.env[key] })
			if ns != tt.wantNS || kctx != tt.wantContext {
				t.Fatalf("targetFrom() = (%q, %q), want (%q, %q)", ns, kctx, tt.wantNS, tt.wantContext)
			}
		})
	}
}

func TestPodTargetConstants(t *testing.T) {
	if PodTmuxSession != "main" {
		t.Fatalf("PodTmuxSession = %q, want %q", PodTmuxSession, "main")
	}
	if PodTmuxSession != tmuxSession {
		t.Fatalf("PodTmuxSession = %q, want the provider's tmux session %q", PodTmuxSession, tmuxSession)
	}
	if AgentContainer != "agent" {
		t.Fatalf("AgentContainer = %q, want %q", AgentContainer, "agent")
	}
}

func TestBuildPodNamesAgentContainer(t *testing.T) {
	pod, err := buildPod("pod-worker", runtime.Config{Command: "/bin/bash"}, newProviderWithOps(newFakeK8sOps()))
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	if len(pod.Spec.Containers) == 0 || pod.Spec.Containers[0].Name != AgentContainer {
		t.Fatalf("pod containers = %+v, want first container %q", pod.Spec.Containers, AgentContainer)
	}
}
