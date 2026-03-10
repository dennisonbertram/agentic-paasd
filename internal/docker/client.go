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

// RunContainer creates and starts a container with gVisor runtime.
func (c *Client) RunContainer(ctx context.Context, tenantID, serviceID, img string, port int, envVars map[string]string, extraLabels map[string]string) (string, error) {
	name := containerName(tenantID, serviceID)

	env := make([]string, 0, len(envVars))
	for k, v := range envVars {
		env = append(env, k+"="+v)
	}

	// Default port for Traefik routing
	if port <= 0 {
		port = 8000
	}

	labels := map[string]string{
		"traefik.enable": "true",
		fmt.Sprintf("traefik.http.routers.%s.rule", serviceID):                            fmt.Sprintf("Host(`%s.localhost`)", serviceID),
		fmt.Sprintf("traefik.http.routers.%s.entrypoints", serviceID):                     "web",
		fmt.Sprintf("traefik.http.services.%s.loadbalancer.server.port", serviceID):        fmt.Sprintf("%d", port),
		"paasd.tenant":  tenantID,
		"paasd.service": serviceID,
	}
	for k, v := range extraLabels {
		labels[k] = v
	}

	hostCfg := &container.HostConfig{
		Runtime: "runsc",
		Resources: container.Resources{
			Memory:   512 * 1024 * 1024, // 512MB
			NanoCPUs: 1_000_000_000,     // 1 CPU
		},
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		NetworkMode:   "traefik-public",
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

	if err := c.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Attempt cleanup on start failure
		_ = c.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("start container: %w", err)
	}

	return resp.ID, nil
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
	// Drain the reader to complete the pull
	_, _ = io.Copy(io.Discard, reader)
	return nil
}

// containerName generates a deterministic container name from tenant and service IDs.
func containerName(tenantID, serviceID string) string {
	tid := tenantID
	if len(tid) > 8 {
		tid = tid[:8]
	}
	sid := serviceID
	if len(sid) > 8 {
		sid = sid[:8]
	}
	return fmt.Sprintf("paasd-%s-%s", tid, sid)
}
