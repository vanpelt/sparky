// Package daemon implements the hiveqa daemon which orchestrates
// compose stacks and their tsnet proxy instances.
package daemon

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/hiveqa/internal/api"
	"github.com/vanpelt/sparky/tools/hiveqa/internal/compose"
	"github.com/vanpelt/sparky/tools/hiveqa/internal/config"
	"github.com/vanpelt/sparky/tools/hiveqa/internal/proxy"
)

// Stack represents a running compose stack with its proxy.
type Stack struct {
	Name          string
	ComposePath   string // original compose file
	RewrittenPath string // hiveqa-modified compose file
	ProjectName   string // docker compose project name
	Services      []compose.ServicePort
	Proxy         *proxy.Instance
	Status        string
}

// Daemon manages the lifecycle of hiveqa stacks.
type Daemon struct {
	cfg      *config.Config
	stacks   map[string]*Stack
	mu       sync.Mutex
	tlsConf  *tls.Config // shared ACME tls.Config, nil when not using ACME
}

// New creates a new Daemon with the given configuration.
func New(cfg *config.Config) (*Daemon, error) {
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "stacks"), 0700); err != nil {
		return nil, fmt.Errorf("creating state dir: %w", err)
	}
	return &Daemon{
		cfg:    cfg,
		stacks: make(map[string]*Stack),
	}, nil
}

// SetTLSConfig sets the shared TLS config (from ACME cert manager).
func (d *Daemon) SetTLSConfig(tc *tls.Config) {
	d.tlsConf = tc
}

// Up brings up a new stack: rewrites compose, starts containers, starts proxy.
func (d *Daemon) Up(ctx context.Context, req api.UpRequest) (*api.StackInfo, error) {
	d.mu.Lock()
	if _, exists := d.stacks[req.Name]; exists {
		d.mu.Unlock()
		return nil, fmt.Errorf("stack %q already running", req.Name)
	}
	d.mu.Unlock()

	projectName := "hiveqa-" + req.Name
	stackDir := filepath.Join(d.cfg.StateDir, "stacks", req.Name)

	// 1. Rewrite compose file.
	result, err := compose.Rewrite(req.ComposePath, req.Name, stackDir)
	if err != nil {
		return nil, fmt.Errorf("rewriting compose: %w", err)
	}

	// If services were explicitly specified, filter to those.
	services := result.Services
	if len(req.Services) > 0 {
		services = filterServices(result.Services, req.Services)
	}
	if len(services) == 0 {
		return nil, fmt.Errorf("no services with ports found in compose file")
	}

	// 2. Start compose stack.
	log.Printf("[%s] starting compose stack...", req.Name)
	cmd := exec.CommandContext(ctx, "docker", "compose",
		"-f", result.OutputPath,
		"-p", projectName,
		"up", "-d",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker compose up: %w", err)
	}

	// 3. Discover container IPs.
	targets, err := discoverTargets(ctx, projectName, services)
	if err != nil {
		rollback(projectName, result.OutputPath)
		return nil, fmt.Errorf("discovering containers: %w", err)
	}

	// 4. Start tsnet proxy.
	tsnetDir := filepath.Join(stackDir, "tsnet")
	if err := os.MkdirAll(tsnetDir, 0700); err != nil {
		rollback(projectName, result.OutputPath)
		return nil, fmt.Errorf("creating tsnet dir: %w", err)
	}

	instCfg := proxy.InstanceConfig{
		Name:       req.Name,
		StateDir:   tsnetDir,
		Targets:    targets,
		ControlURL: d.cfg.ControlURL,
		TLSMode:    proxy.TLSMode(d.cfg.TLS),
		TLSConfig:  d.tlsConf,
	}

	inst := proxy.NewInstance(instCfg)
	if err := inst.Start(ctx); err != nil {
		rollback(projectName, result.OutputPath)
		return nil, fmt.Errorf("starting proxy: %w", err)
	}

	stack := &Stack{
		Name:          req.Name,
		ComposePath:   req.ComposePath,
		RewrittenPath: result.OutputPath,
		ProjectName:   projectName,
		Services:      services,
		Proxy:         inst,
		Status:        "running",
	}

	d.mu.Lock()
	d.stacks[req.Name] = stack
	d.mu.Unlock()

	d.saveMeta(stack)

	svcNames := make([]string, len(services))
	for i, s := range services {
		svcNames[i] = fmt.Sprintf("%s:%d", s.Service, s.ContainerPort)
	}

	info := &api.StackInfo{
		Name:        req.Name,
		ComposePath: req.ComposePath,
		Status:      "running",
		Hostname:    req.Name,
		URL:         inst.URL(),
		Services:    svcNames,
	}
	return info, nil
}

// Down tears down a running stack.
func (d *Daemon) Down(name string) error {
	d.mu.Lock()
	stack, exists := d.stacks[name]
	if !exists {
		d.mu.Unlock()
		return fmt.Errorf("stack %q not found", name)
	}
	delete(d.stacks, name)
	d.mu.Unlock()

	if stack.Proxy != nil {
		stack.Proxy.Stop()
	}

	log.Printf("[%s] stopping compose stack...", name)
	cmd := exec.Command("docker", "compose",
		"-f", stack.RewrittenPath,
		"-p", stack.ProjectName,
		"down", "-v",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Printf("[%s] warning: compose down failed: %v", name, err)
	}

	stackDir := filepath.Join(d.cfg.StateDir, "stacks", name)
	os.RemoveAll(stackDir)

	return nil
}

// List returns info about all running stacks.
func (d *Daemon) List() []api.StackInfo {
	d.mu.Lock()
	defer d.mu.Unlock()

	var result []api.StackInfo
	for _, stack := range d.stacks {
		svcNames := make([]string, len(stack.Services))
		for i, s := range stack.Services {
			svcNames[i] = fmt.Sprintf("%s:%d", s.Service, s.ContainerPort)
		}

		urlStr := ""
		if stack.Proxy != nil {
			urlStr = stack.Proxy.URL()
		}

		result = append(result, api.StackInfo{
			Name:        stack.Name,
			ComposePath: stack.ComposePath,
			Status:      stack.Status,
			Hostname:    stack.Name,
			URL:         urlStr,
			Services:    svcNames,
		})
	}
	return result
}

// Shutdown gracefully stops all stacks.
func (d *Daemon) Shutdown() {
	d.mu.Lock()
	names := make([]string, 0, len(d.stacks))
	for name := range d.stacks {
		names = append(names, name)
	}
	d.mu.Unlock()

	for _, name := range names {
		log.Printf("shutting down stack %q...", name)
		if err := d.Down(name); err != nil {
			log.Printf("error shutting down %q: %v", name, err)
		}
	}
}

// ServeControl starts the HTTP-over-unix-socket control API.
func (d *Daemon) ServeControl(ctx context.Context, socketPath string) error {
	os.Remove(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", socketPath, err)
	}
	os.Chmod(socketPath, 0600)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /up", d.handleUp)
	mux.HandleFunc("POST /down", d.handleDown)
	mux.HandleFunc("GET /list", d.handleList)

	srv := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		srv.Close()
		ln.Close()
	}()

	log.Printf("control socket listening on %s", socketPath)
	return srv.Serve(ln)
}

func (d *Daemon) handleUp(w http.ResponseWriter, r *http.Request) {
	var req api.UpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, api.Response{Error: err.Error()})
		return
	}

	info, err := d.Up(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, api.Response{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, api.Response{OK: true, Data: info})
}

func (d *Daemon) handleDown(w http.ResponseWriter, r *http.Request) {
	var req api.DownRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, api.Response{Error: err.Error()})
		return
	}

	if err := d.Down(req.Name); err != nil {
		writeJSON(w, http.StatusInternalServerError, api.Response{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, api.Response{OK: true})
}

func (d *Daemon) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.Response{OK: true, Data: d.List()})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func discoverTargets(ctx context.Context, projectName string, services []compose.ServicePort) ([]proxy.Target, error) {
	networkName := projectName + "_default"

	var targets []proxy.Target
	for attempt := 0; attempt < 15; attempt++ {
		targets = nil
		allFound := true

		for _, svc := range services {
			containerName := fmt.Sprintf("%s-%s-1", projectName, svc.Service)
			ip, err := getContainerIP(ctx, containerName, networkName)
			if err != nil || ip == "" {
				allFound = false
				break
			}
			targets = append(targets, proxy.Target{
				Service:     svc.Service,
				ContainerIP: ip,
				Port:        svc.ContainerPort,
			})
		}

		if allFound {
			return targets, nil
		}
		time.Sleep(time.Second)
	}

	return nil, fmt.Errorf("timed out waiting for container IPs on network %s", networkName)
}

func getContainerIP(ctx context.Context, containerName, networkName string) (string, error) {
	tmpl := fmt.Sprintf("{{(index .NetworkSettings.Networks %q).IPAddress}}", networkName)
	cmd := exec.CommandContext(ctx, "docker", "inspect", "--format", tmpl, containerName)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func filterServices(all []compose.ServicePort, requested []api.ServiceMapping) []compose.ServicePort {
	want := make(map[string]int)
	for _, r := range requested {
		want[r.Name] = r.Port
	}

	var result []compose.ServicePort
	for _, s := range all {
		if port, ok := want[s.Service]; ok {
			if port > 0 {
				s.ContainerPort = port
			}
			result = append(result, s)
		}
	}

	existing := make(map[string]bool)
	for _, r := range result {
		existing[r.Service] = true
	}
	for _, r := range requested {
		if !existing[r.Name] && r.Port > 0 {
			result = append(result, compose.ServicePort{
				Service:       r.Name,
				ContainerPort: r.Port,
				Protocol:      "tcp",
			})
		}
	}
	return result
}

func rollback(projectName, composePath string) {
	log.Printf("rolling back compose stack %s...", projectName)
	cmd := exec.Command("docker", "compose", "-f", composePath, "-p", projectName, "down", "-v")
	cmd.Run()
}

type stackMeta struct {
	Name        string               `json:"name"`
	ComposePath string               `json:"compose_path"`
	Services    []compose.ServicePort `json:"services"`
}

func (d *Daemon) saveMeta(stack *Stack) {
	meta := stackMeta{
		Name:        stack.Name,
		ComposePath: stack.ComposePath,
		Services:    stack.Services,
	}
	data, _ := json.MarshalIndent(meta, "", "  ")
	metaPath := filepath.Join(d.cfg.StateDir, "stacks", stack.Name, "meta.json")
	os.WriteFile(metaPath, data, 0644)
}
