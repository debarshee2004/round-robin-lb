package main

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// Server represents a backend server
type Server struct {
	URL          *url.URL
	Alive        bool
	mux          sync.RWMutex
	ReverseProxy *httputil.ReverseProxy
}

// SetAlive sets the server's alive status in a thread-safe manner
func (s *Server) SetAlive(alive bool) {
	s.mux.Lock()
	s.Alive = alive
	s.mux.Unlock()
}

// IsAlive returns the server's alive status in a thread-safe manner
func (s *Server) IsAlive() bool {
	s.mux.RLock()
	alive := s.Alive
	s.mux.RUnlock()
	return alive
}

// ServerPool represents a pool of backend servers
type ServerPool struct {
	servers []*Server
	current uint64
}

// AddServer adds a new server to the pool
func (sp *ServerPool) AddServer(server *Server) {
	sp.servers = append(sp.servers, server)
}

// NextIndex returns the next server index using round-robin algorithm
func (sp *ServerPool) NextIndex() int {
	return int(atomic.AddUint64(&sp.current, uint64(1)) % uint64(len(sp.servers)))
}

// MarkServerStatus marks a server as alive or dead
func (sp *ServerPool) MarkServerStatus(serverURL *url.URL, alive bool) {
	for _, server := range sp.servers {
		if server.URL.String() == serverURL.String() {
			server.SetAlive(alive)
			break
		}
	}
}

// GetNextPeer returns the next available server using round-robin
func (sp *ServerPool) GetNextPeer() *Server {
	// Get the next server index
	next := sp.NextIndex()

	// Get the total number of servers
	length := len(sp.servers) + next

	// Loop through servers starting from the next index
	for i := next; i < length; i++ {
		// Use modulo to wrap around
		idx := i % len(sp.servers)

		// If server is alive, return it
		if sp.servers[idx].IsAlive() {
			// Update current to this server's index
			if i != next {
				atomic.StoreUint64(&sp.current, uint64(idx))
			}
			return sp.servers[idx]
		}
	}

	// If no servers are alive, return nil
	return nil
}

// GetServersList returns all servers in the pool
func (sp *ServerPool) GetServersList() []*Server {
	return sp.servers
}

// LoadBalancer represents the main load balancer
type LoadBalancer struct {
	port       string
	serverPool *ServerPool
}

// NewLoadBalancer creates a new load balancer instance
func NewLoadBalancer(port string) *LoadBalancer {
	return &LoadBalancer{
		port:       port,
		serverPool: &ServerPool{},
	}
}

// AddServer adds a server to the load balancer
func (lb *LoadBalancer) AddServer(serverURL string) error {
	url, err := url.Parse(serverURL)
	if err != nil {
		return fmt.Errorf("failed to parse server URL %s: %w", serverURL, err)
	}

	// Create reverse proxy for the server
	proxy := httputil.NewSingleHostReverseProxy(url)

	// Customize the proxy's error handler
	proxy.ErrorHandler = func(writer http.ResponseWriter, request *http.Request, err error) {
		log.Printf("Error proxying request to %s: %v", url.Host, err)

		// Mark server as dead
		lb.serverPool.MarkServerStatus(url, false)

		// Try to get another server
		retries := 3
		for i := 0; i < retries; i++ {
			peer := lb.serverPool.GetNextPeer()
			if peer != nil {
				peer.ReverseProxy.ServeHTTP(writer, request)
				return
			}
		}

		// If no servers available, return 503
		http.Error(writer, "Service unavailable", http.StatusServiceUnavailable)
	}

	server := &Server{
		URL:          url,
		Alive:        true,
		ReverseProxy: proxy,
	}

	lb.serverPool.AddServer(server)
	log.Printf("Added server: %s", serverURL)
	return nil
}

// ServeHTTP handles incoming HTTP requests
func (lb *LoadBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Get the next available server
	peer := lb.serverPool.GetNextPeer()
	if peer != nil {
		// Log the request
		log.Printf("Forwarding request to: %s", peer.URL.String())

		// Forward the request to the selected server
		peer.ReverseProxy.ServeHTTP(w, r)
		return
	}

	// If no servers are available, return 503
	http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
}

// Start starts the load balancer server
func (lb *LoadBalancer) Start() error {
	server := &http.Server{
		Addr:    fmt.Sprintf(":%s", lb.port),
		Handler: lb,
	}

	log.Printf("Load balancer started on port %s", lb.port)

	// Start health check in a separate goroutine
	go lb.healthCheck()

	return server.ListenAndServe()
}

// healthCheck performs periodic health checks on all servers
func (lb *LoadBalancer) healthCheck() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			log.Println("Starting health check...")
			lb.performHealthCheck()
		}
	}
}

// performHealthCheck checks the health of all servers
func (lb *LoadBalancer) performHealthCheck() {
	for _, server := range lb.serverPool.GetServersList() {
		status := "alive"
		alive := isServerAlive(server.URL)
		server.SetAlive(alive)

		if !alive {
			status = "dead"
		}

		log.Printf("Server %s is %s", server.URL.String(), status)
	}
}

// isServerAlive checks if a server is responding
func isServerAlive(url *url.URL) bool {
	timeout := 2 * time.Second
	conn, err := http.NewRequest("GET", url.String(), nil)
	if err != nil {
		return false
	}

	client := &http.Client{
		Timeout: timeout,
	}

	resp, err := client.Do(conn)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

// GetStats returns statistics about the load balancer
func (lb *LoadBalancer) GetStats() map[string]interface{} {
	stats := make(map[string]interface{})

	var aliveCount, deadCount int
	servers := make([]map[string]interface{}, 0)

	for _, server := range lb.serverPool.GetServersList() {
		serverInfo := map[string]interface{}{
			"url":   server.URL.String(),
			"alive": server.IsAlive(),
		}
		servers = append(servers, serverInfo)

		if server.IsAlive() {
			aliveCount++
		} else {
			deadCount++
		}
	}

	stats["total_servers"] = len(lb.serverPool.GetServersList())
	stats["alive_servers"] = aliveCount
	stats["dead_servers"] = deadCount
	stats["servers"] = servers

	return stats
}

// SetupStatsEndpoint sets up an endpoint to view load balancer statistics
func (lb *LoadBalancer) SetupStatsEndpoint() {
	http.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		stats := lb.GetStats()
		w.Header().Set("Content-Type", "application/json")

		response := fmt.Sprintf(`{
			"total_servers": %d,
			"alive_servers": %d,
			"dead_servers": %d,
			"current_index": %d,
			"servers": [`,
			stats["total_servers"],
			stats["alive_servers"],
			stats["dead_servers"],
			atomic.LoadUint64(&lb.serverPool.current))

		servers := stats["servers"].([]map[string]interface{})
		for i, server := range servers {
			if i > 0 {
				response += ","
			}
			response += fmt.Sprintf(`
			{
				"url": "%s",
				"alive": %t
			}`, server["url"], server["alive"])
		}

		response += `
			]
		}`

		w.Write([]byte(response))
	})
}

// Simple test server for demonstration
func createTestServer(port string) {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		response := fmt.Sprintf("Hello from server on port %s! Path: %s", port, r.URL.Path)
		w.Write([]byte(response))
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	server := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	log.Printf("Test server started on port %s", port)
	log.Fatal(server.ListenAndServe())
}

func main() {
	// Create load balancer
	lb := NewLoadBalancer("8080")

	// Add backend servers
	servers := []string{
		"http://localhost:8081",
		"http://localhost:8082",
		"http://localhost:8083",
	}

	for _, serverURL := range servers {
		if err := lb.AddServer(serverURL); err != nil {
			log.Fatalf("Failed to add server %s: %v", serverURL, err)
		}
	}

	// Setup statistics endpoint
	lb.SetupStatsEndpoint()

	// Start test servers in separate goroutines (for demonstration)
	go createTestServer("8081")
	go createTestServer("8082")
	go createTestServer("8083")

	// Give test servers time to start
	time.Sleep(1 * time.Second)

	// Print usage information
	fmt.Println("Load Balancer is running on port 8080")
	fmt.Println("Backend servers:")
	for _, server := range servers {
		fmt.Printf("  - %s\n", server)
	}
	fmt.Println("\nEndpoints:")
	fmt.Println("  - http://localhost:8080/ (load balanced)")
	fmt.Println("  - http://localhost:8080/stats (statistics)")
	fmt.Println("\nPress Ctrl+C to stop")

	// Start the load balancer
	if err := lb.Start(); err != nil {
		log.Fatalf("Failed to start load balancer: %v", err)
	}
}
