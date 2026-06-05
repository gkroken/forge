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
func StartForge(t *testing.T) *Server { return StartForgeEnv(t, nil) }

// StartForgeEnv is like StartForge but appends extraEnv entries (KEY=VALUE) to
// the forge subprocess environment. Use this to override repo upstreams in tests
// that need a controllable mock server (e.g. CRAN_PROXY_UPSTREAM=http://...).
func StartForgeEnv(t *testing.T, extraEnv []string) *Server {
	t.Helper()

	tmpDir := t.TempDir()
	binary := filepath.Join(tmpDir, "forge")
	// On Windows, go build appends .exe when the output path has no extension.
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}

	buildCmd := exec.Command("go", "build", "-o", binary, "./cmd/forge") // #nosec G204 -- test harness only
	buildCmd.Dir = projectRoot()
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("conformance: build forge: %v\n%s", err, out)
	}

	port := freePort(t)
	srv := exec.Command(binary, // #nosec G204 -- test harness only; binary is the forge binary built above
		"-addr", fmt.Sprintf(":%d", port),
		"-data", filepath.Join(tmpDir, "data"),
	)
	if len(extraEnv) > 0 {
		srv.Env = append(os.Environ(), extraEnv...)
	}
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

	timeout := 10 * time.Minute
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: image,
			// Override the image's ENTRYPOINT with sh so that images whose
			// entrypoint is a specific binary (e.g. helm, oras) still run our
			// shell script. Cmd provides the arguments to that sh invocation.
			Entrypoint: []string{"sh"},
			Cmd:        []string{"-c", script},
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
	l, err := net.Listen("tcp", ":0") // #nosec G102 -- test port allocation; any interface is intentional
	if err != nil {
		t.Fatalf("conformance: find free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// RunHostRscript runs an R script using the host Rscript binary. Used for
// platform-specific binary conformance tests that cannot run inside a Linux
// Docker container (Windows .zip install, macOS .tgz install). If Rscript is
// not on PATH the test is skipped rather than failed.
func RunHostRscript(t *testing.T, script string) {
	t.Helper()
	rscript, err := exec.LookPath("Rscript")
	if err != nil {
		t.Skip("Rscript not found on PATH")
	}
	f, err := os.CreateTemp("", "forge-rscript-*.R")
	if err != nil {
		t.Fatalf("RunHostRscript: create temp: %v", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(script); err != nil {
		t.Fatalf("RunHostRscript: write: %v", err)
	}
	f.Close()
	cmd := exec.Command(rscript, "--vanilla", f.Name()) // #nosec G204 -- test harness only
	out, err := cmd.CombinedOutput()
	t.Logf("Rscript output:\n%s", out)
	if err != nil {
		t.Fatalf("Rscript exited non-zero: %v", err)
	}
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
