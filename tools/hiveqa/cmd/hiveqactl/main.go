// hiveqactl is the CLI for interacting with the hiveqa daemon.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/vanpelt/sparky/tools/hiveqa/internal/api"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "up":
		cmdUp(args)
	case "down":
		cmdDown(args)
	case "list", "ls":
		cmdList(args)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `hiveqactl — manage hiveqa stacks

Usage:
  hiveqactl up    --name <name> --compose <path> [--service <svc:port>...]
  hiveqactl down  --name <name>
  hiveqactl list
  hiveqactl help
`)
}

func cmdUp(args []string) {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	name := fs.String("name", "", "stack name (becomes the tailnet hostname)")
	composePath := fs.String("compose", "", "path to docker-compose.yml")
	var services serviceFlags
	fs.Var(&services, "service", "service:port mapping (repeatable)")
	fs.Parse(args)

	if *name == "" || *composePath == "" {
		fmt.Fprintln(os.Stderr, "error: --name and --compose are required")
		fs.Usage()
		os.Exit(1)
	}

	// Resolve compose path to absolute.
	absPath, err := filepath.Abs(*composePath)
	if err != nil {
		fatalf("resolving path: %v", err)
	}

	req := api.UpRequest{
		Name:        *name,
		ComposePath: absPath,
		Services:    services.mappings,
	}

	var resp api.Response
	if err := doRequest("POST", "/up", req, &resp); err != nil {
		fatalf("request failed: %v", err)
	}
	if !resp.OK {
		fatalf("error: %s", resp.Error)
	}

	// Print result.
	data, _ := json.Marshal(resp.Data)
	var info api.StackInfo
	json.Unmarshal(data, &info)

	fmt.Printf("Stack %q is up!\n", info.Name)
	fmt.Printf("  URL:      %s\n", info.URL)
	fmt.Printf("  Services: %s\n", strings.Join(info.Services, ", "))
}

func cmdDown(args []string) {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	name := fs.String("name", "", "stack name to tear down")
	fs.Parse(args)

	if *name == "" {
		fmt.Fprintln(os.Stderr, "error: --name is required")
		fs.Usage()
		os.Exit(1)
	}

	req := api.DownRequest{Name: *name}
	var resp api.Response
	if err := doRequest("POST", "/down", req, &resp); err != nil {
		fatalf("request failed: %v", err)
	}
	if !resp.OK {
		fatalf("error: %s", resp.Error)
	}

	fmt.Printf("Stack %q is down.\n", *name)
}

func cmdList(args []string) {
	var resp api.Response
	if err := doRequest("GET", "/list", nil, &resp); err != nil {
		fatalf("request failed: %v", err)
	}
	if !resp.OK {
		fatalf("error: %s", resp.Error)
	}

	data, _ := json.Marshal(resp.Data)
	var stacks []api.StackInfo
	json.Unmarshal(data, &stacks)

	if len(stacks) == 0 {
		fmt.Println("No running stacks.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tURL\tSERVICES")
	for _, s := range stacks {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Name, s.Status, s.URL, strings.Join(s.Services, ", "))
	}
	w.Flush()
}

// serviceFlags implements flag.Value for repeatable --service svc:port flags.
type serviceFlags struct {
	mappings []api.ServiceMapping
}

func (s *serviceFlags) String() string { return "" }

func (s *serviceFlags) Set(val string) error {
	parts := strings.SplitN(val, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid service mapping %q, expected name:port", val)
	}
	var port int
	if _, err := fmt.Sscanf(parts[1], "%d", &port); err != nil {
		return fmt.Errorf("invalid port in %q: %v", val, err)
	}
	s.mappings = append(s.mappings, api.ServiceMapping{Name: parts[0], Port: port})
	return nil
}

func socketPath() string {
	stateDir := os.Getenv("HIVEQA_STATE_DIR")
	if stateDir == "" {
		home, _ := os.UserHomeDir()
		stateDir = filepath.Join(home, ".local", "share", "hiveqa")
	}
	return filepath.Join(stateDir, "hiveqad.sock")
}

func doRequest(method, path string, body interface{}, resp *api.Response) error {
	sock := socketPath()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sock)
			},
		},
	}

	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(data)
	}

	url := "http://hiveqa" + path
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	r, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connecting to daemon (is hiveqad running?): %w", err)
	}
	defer r.Body.Close()

	return json.NewDecoder(r.Body).Decode(resp)
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
