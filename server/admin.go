package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// AgentInfo represents information about a connected agent
type AgentInfo struct {
	ID          string    `json:"id"`
	RemoteAddr  string    `json:"remote_addr"`
	Subdomains  []string  `json:"subdomains"`
	TCPPorts    []int     `json:"tcp_ports"`
	ConnectedAt time.Time `json:"connected_at"`
	Uptime      string    `json:"uptime"`
}

// StatsResponse represents server statistics
type StatsResponse struct {
	Uptime        string `json:"uptime"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	TotalAgents   int    `json:"total_agents"`
	HTTPTunnels   int    `json:"http_tunnels"`
	TCPTunnels    int    `json:"tcp_tunnels"`
}

// ServeAdmin starts the admin HTTP server
func (s *Server) ServeAdmin(ctx context.Context, port int) {
	mux := http.NewServeMux()

	// List all connected agents
	mux.HandleFunc("/api/agents", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		agents := s.listAgents()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(agents)
	})

	// Get or disconnect a specific agent
	mux.HandleFunc("/api/agents/", func(w http.ResponseWriter, r *http.Request) {
		agentID := strings.TrimPrefix(r.URL.Path, "/api/agents/")
		if agentID == "" {
			http.Error(w, "Agent ID required", http.StatusBadRequest)
			return
		}

		switch r.Method {
		case http.MethodGet:
			agent := s.getAgentInfo(agentID)
			if agent == nil {
				http.Error(w, "Agent not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(agent)

		case http.MethodDelete:
			if err := s.disconnectAgent(agentID); err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Get server statistics
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		stats := s.getStats()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats)
	})

	// Root redirects to stats
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/api/stats", http.StatusFound)
	})

	addr := fmt.Sprintf(":%d", port)
	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	s.logger.Info("admin API listening", "addr", addr)

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("admin server error", "error", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(shutdownCtx)
}

// listAgents returns information about all connected agents
func (s *Server) listAgents() []AgentInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Collect unique agents from both registries
	agentMap := make(map[string]*AgentSession)
	for _, agent := range s.httpRegistry {
		agentMap[agent.ID] = agent
	}
	for _, agent := range s.tcpRegistry {
		agentMap[agent.ID] = agent
	}

	agents := make([]AgentInfo, 0, len(agentMap))
	for _, agent := range agentMap {
		agents = append(agents, AgentInfo{
			ID:          agent.ID,
			RemoteAddr:  agent.Conn.RemoteAddr().String(),
			Subdomains:  agent.Subdomains,
			TCPPorts:    agent.TCPPorts,
			ConnectedAt: agent.ConnectedAt,
			Uptime:      time.Since(agent.ConnectedAt).Round(time.Second).String(),
		})
	}

	return agents
}

// getAgentInfo returns information about a specific agent
func (s *Server) getAgentInfo(id string) *AgentInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Search in both registries
	for _, agent := range s.httpRegistry {
		if agent.ID == id {
			return &AgentInfo{
				ID:          agent.ID,
				RemoteAddr:  agent.Conn.RemoteAddr().String(),
				Subdomains:  agent.Subdomains,
				TCPPorts:    agent.TCPPorts,
				ConnectedAt: agent.ConnectedAt,
				Uptime:      time.Since(agent.ConnectedAt).Round(time.Second).String(),
			}
		}
	}
	for _, agent := range s.tcpRegistry {
		if agent.ID == id {
			return &AgentInfo{
				ID:          agent.ID,
				RemoteAddr:  agent.Conn.RemoteAddr().String(),
				Subdomains:  agent.Subdomains,
				TCPPorts:    agent.TCPPorts,
				ConnectedAt: agent.ConnectedAt,
				Uptime:      time.Since(agent.ConnectedAt).Round(time.Second).String(),
			}
		}
	}

	return nil
}

// disconnectAgent forcibly disconnects an agent by ID
func (s *Server) disconnectAgent(id string) error {
	s.mu.RLock()
	var agent *AgentSession

	// Search in both registries
	for _, a := range s.httpRegistry {
		if a.ID == id {
			agent = a
			break
		}
	}
	if agent == nil {
		for _, a := range s.tcpRegistry {
			if a.ID == id {
				agent = a
				break
			}
		}
	}
	s.mu.RUnlock()

	if agent == nil {
		return fmt.Errorf("agent not found")
	}

	// Close the session - this will trigger cleanup via the CloseChan goroutine
	agent.Session.Close()
	s.logger.Info("agent disconnected via admin API", "id", id)

	return nil
}

// getStats returns server statistics
func (s *Server) getStats() StatsResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Count unique agents
	agentMap := make(map[string]struct{})
	for _, agent := range s.httpRegistry {
		agentMap[agent.ID] = struct{}{}
	}
	for _, agent := range s.tcpRegistry {
		agentMap[agent.ID] = struct{}{}
	}

	uptime := time.Since(s.startedAt)

	return StatsResponse{
		Uptime:        uptime.Round(time.Second).String(),
		UptimeSeconds: int64(uptime.Seconds()),
		TotalAgents:   len(agentMap),
		HTTPTunnels:   len(s.httpRegistry),
		TCPTunnels:    len(s.tcpRegistry),
	}
}
