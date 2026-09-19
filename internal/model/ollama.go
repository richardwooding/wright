package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultOllamaHost is probed when OLLAMA_HOST is unset.
const DefaultOllamaHost = "http://localhost:11434"

// ProbeTimeout bounds the Ollama probe so a missing daemon never delays
// startup noticeably.
const ProbeTimeout = 500 * time.Millisecond

// ErrNotLoopback is returned when a probe would leave the machine without the
// user having pointed OLLAMA_HOST somewhere explicitly.
var ErrNotLoopback = errors.New("model: refusing to probe a non-loopback Ollama host (set OLLAMA_HOST to allow it)")

// ProbeOllama lists the tags a local Ollama serves. host defaults to
// OLLAMA_HOST or DefaultOllamaHost. Only loopback hosts are contacted unless
// OLLAMA_HOST is set, which is the "no phone-home" guarantee for the one
// network call wright makes on its own initiative.
func ProbeOllama(ctx context.Context, host string) ([]string, error) {
	explicit := os.Getenv("OLLAMA_HOST") != ""
	if host == "" {
		host = os.Getenv("OLLAMA_HOST")
	}
	if host == "" {
		host = DefaultOllamaHost
	}
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("model: OLLAMA_HOST: %w", err)
	}
	if !explicit && !isLoopback(u.Hostname()) {
		return nil, ErrNotLoopback
	}
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(u.String(), "/")+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model: ollama /api/tags: %s", resp.Status)
	}
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("model: ollama /api/tags: %w", err)
	}
	tags := make([]string, 0, len(body.Models))
	for _, m := range body.Models {
		if m.Name != "" {
			tags = append(tags, m.Name)
		}
	}
	return tags, nil
}

func isLoopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
