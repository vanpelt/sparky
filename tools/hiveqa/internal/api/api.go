// Package api defines shared types for the hiveqa daemon and CLI.
package api

// UpRequest is sent by hiveqactl to bring up a new stack.
type UpRequest struct {
	Name        string           `json:"name"`
	ComposePath string           `json:"compose_path"`
	Services    []ServiceMapping `json:"services,omitempty"`
}

// ServiceMapping maps a compose service name to the container port to proxy.
type ServiceMapping struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

// DownRequest tears down a stack.
type DownRequest struct {
	Name string `json:"name"`
}

// StackInfo is a summary returned by the list endpoint.
type StackInfo struct {
	Name        string   `json:"name"`
	ComposePath string   `json:"compose_path"`
	Status      string   `json:"status"`
	Hostname    string   `json:"hostname"`
	URL         string   `json:"url,omitempty"`
	Services    []string `json:"services"`
}

// Response is the standard API response envelope.
type Response struct {
	OK      bool        `json:"ok"`
	Error   string      `json:"error,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}
