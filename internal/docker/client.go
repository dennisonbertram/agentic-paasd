// Package docker wraps the Docker Engine API for paasd container lifecycle.
package docker

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

// Client wraps the Docker Engine API client with paasd-specific defaults.
type Client struct {
	cli *client.Client
}

// NewClient creates a Docker API client using the default socket.
func NewClient() (*Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Client{cli: cli}, nil
}

// Close releases the Docker client resources.
func (c *Client) Close() error {
	return c.cli.Close()
}

// ContainerInfo holds inspected container state.
type ContainerInfo struct {
	Status    string
	StartedAt string
	ExitCode  int
}

// EnsureNetwork creates a Docker network if it doesn't exist. Returns the network ID.
func (c *Client) EnsureNetwork(ctx context.Context, name string) (string, error) {
	networks, err := c.cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list networks: %w", err)
	}
	for _, n := range networks {
		if n.Name == name {
			return n.ID, nil
		}
	}
	// Internal bridge with ICC disabled:
	// - Internal: no outbound internet access from containers
	// - ICC disabled: containers on the same bridge cannot communicate with each other
	// - Traefik is connected to this network for ingress routing
	resp, err := c.cli.NetworkCreate(ctx, name, network.CreateOptions{
		Driver:   "bridge",
		Internal: true,
		Options: map[string]string{
			"com.docker.network.bridge.enable_icc": "false",
		},
	})
	if err != nil {
		return "", fmt.Errorf("create network %s: %w", name, err)
	}
	return resp.ID, nil
}

// ConnectNetwork connects a container to an additional network.
func (c *Client) ConnectNetwork(ctx context.Context, networkID, containerID string) error {
	return c.cli.NetworkConnect(ctx, networkID, containerID, nil)
}

// TenantNetworkName returns the deterministic network name for a tenant.
func TenantNetworkName(tenantID string) string {
	return "paasd-tenant-" + tenantID
}

// ResourceLimits holds per-container resource constraints from tenant quotas.
type ResourceLimits struct {
	MemoryMB int64
	CPUCores float64
}

// RunContainer creates and starts a container with gVisor runtime.
//
// Network isolation architecture:
//   - Container is placed on a per-tenant internal Docker network (Internal=true, ICC=false).
//   - Internal network blocks all outbound internet access from containers.
//   - ICC=false prevents containers on the same tenant bridge from communicating.
//   - Traefik is connected to each per-tenant network for ingress routing.
//   - Cross-tenant isolation: each tenant has a separate bridge network.
//   - Defense-in-depth: gVisor (runsc) runtime, CapDrop ALL, no-new-privileges,
//     ReadonlyRootfs, PidsLimit, MemorySwap=Memory (no swap).
func (c *Client) RunContainer(ctx context.Context, tenantID, serviceID, img string, port int, envVars map[string]string, extraLabels map[string]string, limits *ResourceLimits) (string, error) {
	name := containerName(tenantID, serviceID)

	env := make([]string, 0, len(envVars))
	for k, v := range envVars {
		env = append(env, k+"="+v)
	}

	if port <= 0 {
		port = 8000
	}

	labels := map[string]string{
		"traefik.enable": "true",
		fmt.Sprintf("traefik.http.routers.%s.rule", serviceID):                     fmt.Sprintf("Host(`%s.localhost`)", serviceID),
		fmt.Sprintf("traefik.http.routers.%s.entrypoints", serviceID):              "web",
		fmt.Sprintf("traefik.http.services.%s.loadbalancer.server.port", serviceID): fmt.Sprintf("%d", port),
		"paasd.tenant":  tenantID,
		"paasd.service": serviceID,
	}
	for k, v := range extraLabels {
		labels[k] = v
	}

	tenantNet := TenantNetworkName(tenantID)

	// Apply resource limits from tenant quotas, with safe defaults
	memoryBytes := int64(512 * 1024 * 1024) // default 512MB
	nanoCPUs := int64(1_000_000_000)        // default 1 CPU
	if limits != nil {
		if limits.MemoryMB > 0 {
			memoryBytes = limits.MemoryMB * 1024 * 1024
		}
		if limits.CPUCores > 0 {
			nanoCPUs = int64(limits.CPUCores * 1_000_000_000)
		}
	}

	hostCfg := &container.HostConfig{
		Runtime: "runsc",
		Resources: container.Resources{
			Memory:     memoryBytes,
			NanoCPUs:   nanoCPUs,
			PidsLimit:  int64Ptr(256),
			MemorySwap: memoryBytes, // no swap
		},
		RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		NetworkMode:    container.NetworkMode(tenantNet),
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: true,
		Tmpfs: map[string]string{
			"/tmp":     "rw,noexec,nosuid,size=64m",
			"/var/run": "rw,noexec,nosuid,size=16m",
		},
	}

	resp, err := c.cli.ContainerCreate(ctx,
		&container.Config{
			Image:  img,
			Env:    env,
			Labels: labels,
		},
		hostCfg,
		&network.NetworkingConfig{},
		nil,
		name,
	)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}

	// Connect Traefik to the per-tenant network for ingress routing.
	// This is idempotent — Docker ignores if already connected.
	// Note: Traefik gains L2 adjacency with tenant containers, but this is
	// standard for Docker+Traefik deployments. The primary isolation boundaries
	// are: gVisor runtime (syscall interception), CapDrop ALL, no-new-privileges,
	// internal network (no outbound), ICC disabled (no inter-container comms).
	traefikID, findErr := c.findTraefikContainer(ctx)
	if findErr == nil && traefikID != "" {
		_ = c.cli.NetworkConnect(ctx, tenantNet, traefikID, nil)
	}

	if err := c.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = c.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("start container: %w", err)
	}

	return resp.ID, nil
}

func int64Ptr(v int64) *int64 { return &v }

// findTraefikContainer finds the Traefik container by image or name.
func (c *Client) findTraefikContainer(ctx context.Context) (string, error) {
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{All: false})
	if err != nil {
		return "", err
	}
	for _, ctr := range containers {
		if strings.Contains(ctr.Image, "traefik") {
			return ctr.ID, nil
		}
		for _, name := range ctr.Names {
			if strings.Contains(name, "traefik") {
				return ctr.ID, nil
			}
		}
	}
	return "", fmt.Errorf("traefik container not found")
}

// StopContainer stops a running container with a 10s timeout.
func (c *Client) StopContainer(ctx context.Context, containerID string) error {
	timeout := 10
	return c.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout})
}

// StartContainer starts a stopped container.
func (c *Client) StartContainer(ctx context.Context, containerID string) error {
	return c.cli.ContainerStart(ctx, containerID, container.StartOptions{})
}

// RemoveContainer force-removes a container.
func (c *Client) RemoveContainer(ctx context.Context, containerID string) error {
	return c.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

// LogsContainer returns a reader for container logs.
func (c *Client) LogsContainer(ctx context.Context, containerID string, follow bool, tail int) (io.ReadCloser, error) {
	tailStr := "all"
	if tail > 0 {
		tailStr = fmt.Sprintf("%d", tail)
	}
	return c.cli.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     follow,
		Tail:       tailStr,
		Timestamps: true,
	})
}

// InspectContainer returns the container's current state.
func (c *Client) InspectContainer(ctx context.Context, containerID string) (*ContainerInfo, error) {
	info, err := c.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("inspect container: %w", err)
	}
	return &ContainerInfo{
		Status:    strings.ToLower(info.State.Status),
		StartedAt: info.State.StartedAt,
		ExitCode:  info.State.ExitCode,
	}, nil
}

// PullImage pulls an image with a 5-minute timeout.
func (c *Client) PullImage(ctx context.Context, img string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	reader, err := c.cli.ImagePull(ctx, img, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", img, err)
	}
	defer reader.Close()
	_, _ = io.Copy(io.Discard, reader)
	return nil
}

// ListContainersByLabel lists containers matching a label filter.
func (c *Client) ListContainersByLabel(ctx context.Context, label, value string) ([]string, error) {
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{
		All: true,
	})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	var ids []string
	for _, ctr := range containers {
		if v, ok := ctr.Labels[label]; ok && v == value {
			ids = append(ids, ctr.ID)
		}
	}
	return ids, nil
}

// containerName generates a deterministic container name from tenant and service IDs.
func containerName(tenantID, serviceID string) string {
	return fmt.Sprintf("paasd-%s-%s", tenantID, serviceID)
}
