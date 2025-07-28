// internal/proxy/proxy.go
package proxy

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/dagger/container-use/internal/ctxio"
	"github.com/dagger/container-use/internal/utils"
	"github.com/r3labs/sse"
)

var logger *log.Logger

func NewLogger() *log.Logger {
	if logger != nil {
		return logger
	}

	logFile, err := os.OpenFile("/tmp/container-use-proxy.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		logger = log.New(os.Stderr, "[PROXY] ", log.LstdFlags)
	} else {
		logger = log.New(logFile, "[PROXY] ", log.LstdFlags)
	}
	return logger
}

const (
	ANTHROPIC_BASE_DOMAIN = "api.anthropic.com"
)

type Server struct {
	httpServer *http.Server
	manager    net.Conn
	encoder    *json.Encoder
	cancel     context.CancelFunc
	toolsQueue *toolSet
	mu         sync.Mutex
}

type toolSet struct {
	mu sync.Mutex
	s  map[string]struct{}
}

func (s *toolSet) Add(key string) {
	s.mu.Lock()
	s.s[key] = struct{}{}
	s.mu.Unlock()
}

func (s *toolSet) Remove(key string) (n int) {
	s.mu.Lock()
	delete(s.s, key)
	n = len(s.s)
	s.mu.Unlock()
	return
}

func (s *toolSet) Clear() map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.s
	s.s = make(map[string]struct{})
	return old
}

func Start(ctx context.Context, proxyAddr, managerAddr string) (*Server, error) {
	logger = NewLogger()
	logger.Printf("Starting proxy on %s", proxyAddr)

	ctx, cancel := context.WithCancel(ctx)

	// Connect to manager if address provided
	var managerConn net.Conn
	if managerAddr != "" {
		conn, err := connectToManager(managerAddr)
		if err != nil {
			logger.Printf("Warning: failed to connect to manager: %v", err)
		} else {
			managerConn = conn
			logger.Printf("Connected to manager at %s", managerAddr)
		}
	}

	// Create server
	server := &Server{
		manager:    managerConn,
		cancel:     cancel,
		toolsQueue: &toolSet{s: make(map[string]struct{})},
	}

	if managerConn != nil {
		server.encoder = json.NewEncoder(managerConn)
	}

	// Create reverse proxy
	proxy := server.createProxy()

	server.httpServer = &http.Server{
		Addr:    proxyAddr,
		Handler: proxy,
	}

	// Start HTTP server
	go func() {
		if err := server.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("Proxy error: %v", err)
		}
	}()

	// Wait for proxy to be ready
	if err := waitForProxy(proxyAddr); err != nil {
		return nil, err
	}

	return server, nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.manager != nil {
		s.manager.Close()
	}
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) createProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport: http.DefaultTransport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{
				Scheme: "https",
				Host:   ANTHROPIC_BASE_DOMAIN,
			})
			pr.Out.Host = ANTHROPIC_BASE_DOMAIN
		},
		ModifyResponse: s.modifyResponse,
	}
}

func (s *Server) modifyResponse(resp *http.Response) error {
	ctx := context.Background()
	body := resp.Body

	// Handle content encoding
	ce := resp.Header.Get("Content-Encoding")
	resp.Header.Del("Content-Encoding")
	switch ce {
	case "br":
		body = io.NopCloser(brotli.NewReader(body))
	case "gzip":
		var err error
		body, err = gzip.NewReader(body)
		if err != nil {
			return err
		}
	case "":
		// No encoding
	default:
		return fmt.Errorf("unhandled Content-Encoding %s", ce)
	}

	// Check content type
	ct := resp.Header.Get("Content-Type")
	if ct != "" {
		mediaType, params, err := mime.ParseMediaType(ct)
		if err != nil {
			return err
		}
		if charset, ok := params["charset"]; ok && charset != "utf-8" {
			return fmt.Errorf("unhandled charset %q", charset)
		}
		ct = mediaType
	}

	// Only process SSE streams
	if ct != "text/event-stream" {
		resp.Body = body
		return nil
	}

	// Fan out the response for processing
	reader := ctxio.NewReaderFanOut(ctx, body, 2)
	resp.Body = reader.Readers[0]

	// Process SSE stream in background
	go s.processSSEStream(reader.Readers[1])

	return nil
}

func (s *Server) processSSEStream(body io.ReadCloser) {
	defer body.Close()

	eventReader := sse.NewEventStreamReader(body)
	msg := new(anthropic.Message)

	encodingBase64 := false
	
	for {
		p, err := eventReader.ReadEvent()
		if err != nil {
			return
		}
		event := utils.M2(processSSEEvent(p, encodingBase64))
		var ev anthropic.MessageStreamEventUnion
		utils.M(json.Unmarshal(event.Data, &ev))
		utils.M(msg.Accumulate(ev))
		if _, ok := ev.AsAny().(anthropic.MessageStopEvent); ok {
			logger.Println("\n\n===MESSAGE COMPLETE===", msg)
			s.handleMessageComplete(msg)
			*msg = anthropic.Message{}
		}
	}
}

func (s *Server) handleMessageComplete(msg *anthropic.Message) {
	// Track tool uses
	var toolUses []struct {
		ID   string
		Name string
	}
	for _, content := range msg.Content {
		if content.Type == "tool_use" {
			s.toolsQueue.Add(content.ID)
			toolUses = append(toolUses, struct {
				ID   string
				Name string
			}{content.ID, content.Name})
			logger.Printf("Tool use detected: %s (%s)", content.ID, content.Name)
		}
	}

	// Prompt is released to user.
	// TODO: what to do if user add prompts to the queue of prompts ?
	if msg.StopReason == anthropic.StopReasonEndTurn {
		logger.Println("acquiring commit lock")
		s.toolsQueue.mu.Lock()
		if len(s.toolsQueue.s) > 0 {
			// Create descriptive commit message
			var message string
			if len(toolUses) == 1 {
				message = fmt.Sprintf("Tool: %s", toolUses[0].Name)
			} else if len(toolUses) > 1 {
				toolNames := make([]string, len(toolUses))
				for i, tu := range toolUses {
					toolNames[i] = tu.Name
				}
				message = fmt.Sprintf("Tools: %s", strings.Join(toolNames, ", "))
			} else {
				message = "Claude response"
			}
			s.sendCommit(message)
		}
		logger.Println("committing", s.toolsQueue.s)
		s.toolsQueue.s = map[string]struct{}{}
		s.toolsQueue.mu.Unlock()
		logger.Println("releasing commit lock")
	}
}

func (s *Server) sendCommit(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.encoder == nil {
		logger.Println("No manager connected, skipping commit")
		return
	}

	msg := struct {
		Action string `json:"Action"`
		Data   string `json:"Data"`
	}{
		Action: "commit",
		Data:   message,
	}

	if err := s.encoder.Encode(msg); err != nil {
		if err == io.EOF {
			s.cancel()
		} else {
			logger.Printf("Failed to send commit: %v", err)
		}
	}
}

func connectToManager(addr string) (net.Conn, error) {
	maxRetries := 10
	backoff := time.Second / 2

	for i := 0; i < maxRetries; i++ {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			return conn, nil
		}
		if i < maxRetries-1 {
			time.Sleep(backoff)
			backoff *= 2
		}
	}

	return nil, fmt.Errorf("failed to connect to manager at %s after %d attempts", addr, maxRetries)
}

func waitForProxy(addr string) error {
	maxRetries := 30
	for i := 0; i < maxRetries; i++ {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		if i < maxRetries-1 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	return fmt.Errorf("proxy failed to start after %d attempts", maxRetries)
}
