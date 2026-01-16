package metric

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/client"
)

// MockDockerClient implements client.CommonAPIClient for testing
type MockDockerClient struct {
	client.CommonAPIClient
	infoFunc             func(ctx context.Context) (system.Info, error)
	nodeListFunc         func(ctx context.Context, options types.NodeListOptions) ([]swarm.Node, error)
	serviceListFunc      func(ctx context.Context, options types.ServiceListOptions) ([]swarm.Service, error)
	taskListFunc         func(ctx context.Context, options types.TaskListOptions) ([]swarm.Task, error)
	containerInspectFunc func(ctx context.Context, containerID string) (container.InspectResponse, error)
	containerStatsFunc   func(ctx context.Context, containerID string, stream bool) (container.StatsResponseReader, error)
}

func (m *MockDockerClient) Info(ctx context.Context) (system.Info, error) {
	return m.infoFunc(ctx)
}

func (m *MockDockerClient) NodeList(ctx context.Context, options types.NodeListOptions) ([]swarm.Node, error) {
	return m.nodeListFunc(ctx, options)
}

func (m *MockDockerClient) ServiceList(ctx context.Context, options types.ServiceListOptions) ([]swarm.Service, error) {
	return m.serviceListFunc(ctx, options)
}

func (m *MockDockerClient) TaskList(ctx context.Context, options types.TaskListOptions) ([]swarm.Task, error) {
	return m.taskListFunc(ctx, options)
}

func (m *MockDockerClient) ContainerInspect(ctx context.Context, containerID string) (container.InspectResponse, error) {
	return m.containerInspectFunc(ctx, containerID)
}

func (m *MockDockerClient) ContainerStats(ctx context.Context, containerID string, stream bool) (container.StatsResponseReader, error) {
	return m.containerStatsFunc(ctx, containerID, stream)
}

func TestCollectSwarmMetrics(t *testing.T) {
	t.Run("Swarm inactive", func(t *testing.T) {
		mock := &MockDockerClient{
			infoFunc: func(ctx context.Context) (system.Info, error) {
				return system.Info{
					Swarm: swarm.Info{
						LocalNodeState: swarm.LocalNodeStateInactive,
					},
				}, nil
			},
		}

		sm, err := collectSwarmMetrics(context.Background(), mock)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if sm.IsSwarm {
			t.Errorf("Expected IsSwarm to be false")
		}
	})

	t.Run("Swarm worker", func(t *testing.T) {
		mock := &MockDockerClient{
			infoFunc: func(ctx context.Context) (system.Info, error) {
				return system.Info{
					Name: "worker-node",
					Swarm: swarm.Info{
						LocalNodeState:   swarm.LocalNodeStateActive,
						NodeID:           "worker-id",
						ControlAvailable: false,
					},
				}, nil
			},
		}

		sm, err := collectSwarmMetrics(context.Background(), mock)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if !sm.IsSwarm {
			t.Errorf("Expected IsSwarm to be true")
		}
		if sm.Role != "worker" {
			t.Errorf("Expected role worker, got %v", sm.Role)
		}
		if sm.NodeID != "worker-id" {
			t.Errorf("Expected NodeID worker-id, got %v", sm.NodeID)
		}
	})

	t.Run("Swarm manager", func(t *testing.T) {
		mock := &MockDockerClient{
			infoFunc: func(ctx context.Context) (system.Info, error) {
				return system.Info{
					Name: "manager-node",
					Swarm: swarm.Info{
						LocalNodeState:   swarm.LocalNodeStateActive,
						NodeID:           "manager-id",
						ControlAvailable: true,
					},
				}, nil
			},
			nodeListFunc: func(ctx context.Context, options types.NodeListOptions) ([]swarm.Node, error) {
				return []swarm.Node{
					{
						ID: "manager-id",
						Description: swarm.NodeDescription{
							Hostname: "manager-node",
						},
						Status: swarm.NodeStatus{
							State: swarm.NodeStateReady,
						},
						Spec: swarm.NodeSpec{
							Role:         swarm.NodeRoleManager,
							Availability: swarm.NodeAvailabilityActive,
						},
						ManagerStatus: &swarm.ManagerStatus{
							Leader:       true,
							Reachability: swarm.ReachabilityReachable,
						},
					},
				}, nil
			},
			serviceListFunc: func(ctx context.Context, options types.ServiceListOptions) ([]swarm.Service, error) {
				replicas := uint64(3)
				return []swarm.Service{
					{
						ID: "svc-id",
						Spec: swarm.ServiceSpec{
							Annotations: swarm.Annotations{
								Name: "my-service",
							},
							Mode: swarm.ServiceMode{
								Replicated: &swarm.ReplicatedService{
									Replicas: &replicas,
								},
							},
							TaskTemplate: swarm.TaskSpec{
								ContainerSpec: &swarm.ContainerSpec{
									Image: "my-image:latest",
								},
							},
						},
					},
				}, nil
			},
			taskListFunc: func(ctx context.Context, options types.TaskListOptions) ([]swarm.Task, error) {
				return []swarm.Task{
					{
						ServiceID: "svc-id",
						Status:    swarm.TaskStatus{State: swarm.TaskStateRunning},
					},
					{
						ServiceID: "svc-id",
						Status:    swarm.TaskStatus{State: swarm.TaskStateRunning},
					},
					{
						ServiceID: "svc-id",
						Status:    swarm.TaskStatus{State: swarm.TaskStateShutdown},
					},
				}, nil
			},
		}

		sm, err := collectSwarmMetrics(context.Background(), mock)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if !sm.IsSwarm {
			t.Errorf("Expected IsSwarm to be true")
		}
		if sm.Role != "manager" {
			t.Errorf("Expected role manager, got %v", sm.Role)
		}
		if len(sm.Nodes) != 1 {
			t.Errorf("Expected 1 node, got %v", len(sm.Nodes))
		}
		if len(sm.Services) != 1 {
			t.Errorf("Expected 1 service, got %v", len(sm.Services))
		}
		if sm.Services[0].RunningTasks != 2 {
			t.Errorf("Expected 2 running tasks, got %v", sm.Services[0].RunningTasks)
		}
	})
}

func TestProcessContainerWithSwarmLabels(t *testing.T) {
	mock := &MockDockerClient{}

	ctx := context.Background()
	containerID := "test-container"

	mock.containerInspectFunc = func(ctx context.Context, containerID string) (container.InspectResponse, error) {
		return container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{
				ID: containerID,
				State: &container.State{
					Status:  "running",
					Running: true,
				},
			},
			Config: &container.Config{
				Labels: map[string]string{
					"com.docker.swarm.node.id":    "node-123",
					"com.docker.swarm.service.id": "svc-456",
					"com.docker.swarm.task.id":    "task-789",
				},
			},
		}, nil
	}

	mockStats := dockerStatsResponse{}
	statsJSON, _ := json.Marshal(mockStats)

	mock.containerStatsFunc = func(ctx context.Context, containerID string, stream bool) (container.StatsResponseReader, error) {
		return container.StatsResponseReader{
			Body: io.NopCloser(bytes.NewReader(statsJSON)),
		}, nil
	}

	cSummary := container.Summary{
		ID:    containerID,
		Names: []string{"/my-container"},
		Image: "my-image",
	}

	metrics, customErr := processContainer(ctx, mock, cSummary)
	if customErr.Error != "" {
		t.Fatalf("Unexpected error: %v", customErr.Error)
	}

	if metrics.Swarm == nil {
		t.Fatal("Expected Swarm info to be present")
	}

	if metrics.Swarm.NodeID != "node-123" {
		t.Errorf("Expected NodeID node-123, got %v", metrics.Swarm.NodeID)
	}
	if metrics.Swarm.ServiceID != "svc-456" {
		t.Errorf("Expected ServiceID svc-456, got %v", metrics.Swarm.ServiceID)
	}
	if metrics.Swarm.TaskID != "task-789" {
		t.Errorf("Expected TaskID task-789, got %v", metrics.Swarm.TaskID)
	}
}