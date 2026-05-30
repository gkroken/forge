// Package conformance provides the harness for end-to-end conformance tests
// that drive real package-manager clients against a live forge instance.
//
// Conformance tests live in this package with the //go:build conformance tag
// and are run with:
//
//	make test/conformance
//
// The harness starts forge as a subprocess (not a container) and runs clients
// in Docker containers that reach forge via host.docker.internal. Requires
// Docker 20.10+ and internet access for proxy tests.
package conformance

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Server represents a running forge instance and exposes helpers for building
// repository URLs for use in test scripts.
type Server struct {
	BaseURL string
}

// Repo returns the base URL for the named repository, e.g.
// "http://host.docker.internal:8080/repository/npm-proxy/".
// Use ContainerRepo when the URL will be used from inside a container.
func (s *Server) Repo(name string) string {
	return s.BaseURL + "/repository/" + name + "/"
}

// ContainerHost returns "host.docker.internal:PORT" — the authority by which
// containers reach the forge server running on the host.
func (s *Server) ContainerHost() string {
	_, port, _ := net.SplitHostPort(s.BaseURL[len("http://"):])
	return "host.docker.internal:" + port
}

// ContainerRepo returns the repository URL reachable from inside a Docker
// container (host.docker.internal instead of localhost).
func (s *Server) ContainerRepo(name string) string {
	return "http://" + s.ContainerHost() + "/repository/" + name + "/"
}

// StartForge builds the forge binary from source, starts it with filesystem
// backends on a free port, and returns a Server. The process is killed when t
// finishes.
func StartForge(t *testing.T) *Server {
	t.Helper()

	tmpDir := t.TempDir()
	binary := filepath.Join(tmpDir, "forge")

	buildCmd := exec.Command("go", "build", "-o", binary, "./cmd/forge")
	buildCmd.Dir = projectRoot()
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("conformance: build forge: %v\n%s", err, out)
	}

	port := freePort(t)
	srv := exec.Command(binary,
		"-addr", fmt.Sprintf(":%d", port),
		"-data", filepath.Join(tmpDir, "data"),
	)
	if err := srv.Start(); err != nil {
		t.Fatalf("conformance: start forge: %v", err)
	}
	t.Cleanup(func() {
		srv.Process.Kill() //nolint:errcheck
		srv.Wait()         //nolint:errcheck
	})

	base := fmt.Sprintf("http://localhost:%d", port)
	waitForReady(t, base+"/healthz", 10*time.Second)
	return &Server{BaseURL: base}
}

// RunScript runs a sh -c script inside a Docker container. The container can
// reach forge via host.docker.internal (mapped with Docker's host-gateway).
// On script failure the container logs are printed and the test is failed.
func RunScript(t *testing.T, image, script string) {
	t.Helper()
	ctx := context.Background()

	timeout := 3 * time.Minute
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: image,
			Cmd:   []string{"sh", "-c", script},
			// Maps host.docker.internal to the host's network gateway so
			// containers can reach forge running on the host.
			ExtraHosts: []string{"host.docker.internal:host-gateway"},
			WaitingFor: wait.ForExit().WithExitTimeout(timeout),
		},
		Started: true,
	})
	if c != nil {
		defer c.Terminate(ctx) //nolint:errcheck
	}
	if err != nil {
		t.Fatalf("conformance: run container: %v", err)
	}

	state, err := c.State(ctx)
	if err != nil {
		t.Fatalf("conformance: container state: %v", err)
	}
	if state.ExitCode != 0 {
		logs, _ := c.Logs(ctx)
		b, _ := io.ReadAll(logs)
		t.Fatalf("conformance: script exited %d\n%s", state.ExitCode, b)
	}
}

// IsReachable returns true if url responds within 5 seconds with a non-5xx
// status. Used to gate tests that require live upstream registries.
func IsReachable(url string) bool {
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}

// projectRoot walks up from this file's directory to find go.mod.
func projectRoot() string {
	_, f, _, _ := runtime.Caller(0)
	dir := filepath.Dir(f)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("conformance: could not find project root")
		}
		dir = parent
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("conformance: find free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func waitForReady(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	cl := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if resp, err := cl.Get(url); err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("conformance: forge at %s not ready after %v", url, timeout)
}
