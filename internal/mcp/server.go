// Package mcp implements the MCP server for Claude Code integration.
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/lokashrinav/codesearch/internal/query"
	"github.com/lokashrinav/codesearch/internal/storage"
)

// Server handles MCP JSON-RPC requests over stdio.
type Server struct {
	engine *query.Engine
	reader *storage.Reader
}

// NewServer creates an MCP server backed by an indexed database.
func NewServer(db *storage.Reader) *Server {
	return &Server{
		engine: query.NewEngine(db),
		reader: db,
	}
}

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type jsonRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Run starts the MCP server, reading JSON-RPC from stdin and writing to stdout.
func (s *Server) Run() error {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.sendError(nil, -32700, "Parse error")
			continue
		}

		s.handleRequest(&req)
	}

	return scanner.Err()
}

func (s *Server) handleRequest(req *jsonRPCRequest) {
	switch req.Method {
	case "initialize":
		s.sendResult(req.ID, map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{},
			},
			"serverInfo": map[string]interface{}{
				"name":    "codesearch",
				"version": "0.1.0",
			},
		})

	case "tools/list":
		s.sendResult(req.ID, map[string]interface{}{
			"tools": toolDefinitions(),
		})

	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		json.Unmarshal(req.Params, &params)
		result := s.callTool(params.Name, params.Arguments)
		s.sendResult(req.ID, result)

	case "notifications/initialized":
		// no-op

	default:
		s.sendError(req.ID, -32601, fmt.Sprintf("Method not found: %s", req.Method))
	}
}

func (s *Server) callTool(name string, args json.RawMessage) interface{} {
	switch name {
	case "codesearch_search":
		var input struct {
			Query   string `json:"query"`
			MaxHops int    `json:"max_hops"`
		}
		json.Unmarshal(args, &input)
		result, err := s.engine.Search(input.Query, input.MaxHops)
		if err != nil {
			return toolError(err.Error())
		}
		return toolResult(result)

	case "codesearch_trace":
		var input struct {
			Symbol    string `json:"symbol"`
			Direction string `json:"direction"`
			MaxHops   int    `json:"max_hops"`
		}
		json.Unmarshal(args, &input)
		result, err := s.engine.Trace(input.Symbol, input.Direction, input.MaxHops)
		if err != nil {
			return toolError(err.Error())
		}
		return toolResult(result)

	case "codesearch_explain":
		var input struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		json.Unmarshal(args, &input)
		result, err := s.engine.Explain(input.From, input.To)
		if err != nil {
			return toolError(err.Error())
		}
		return toolResult(result)

	case "codesearch_field_flow":
		var input struct {
			Type  string `json:"type"`
			Field string `json:"field"`
		}
		json.Unmarshal(args, &input)
		result, err := s.engine.FieldFlow(input.Type, input.Field)
		if err != nil {
			return toolError(err.Error())
		}
		return toolResult(result)

	default:
		return toolError(fmt.Sprintf("Unknown tool: %s", name))
	}
}

func toolDefinitions() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"name":        "codesearch_search",
			"description": "Search for code mechanisms from a symptom description. Bridges the gap between how you describe a problem and where the answer lives in the code.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query":    map[string]string{"type": "string", "description": "Natural language description of the problem, e.g. 'flag did nothing on restore'"},
					"max_hops": map[string]interface{}{"type": "integer", "description": "Maximum graph traversal depth (default 5)"},
				},
				"required": []string{"query"},
			},
		},
		{
			"name":        "codesearch_trace",
			"description": "Trace data flow forward or backward from a specific symbol. Shows where values come from or go to across function boundaries.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"symbol":    map[string]string{"type": "string", "description": "Symbol name to trace"},
					"direction": map[string]string{"type": "string", "description": "'forward' or 'backward' (default: both)"},
					"max_hops":  map[string]interface{}{"type": "integer", "description": "Maximum depth (default 5)"},
				},
				"required": []string{"symbol"},
			},
		},
		{
			"name":        "codesearch_explain",
			"description": "Explain how two symbols are connected through calls, data flow, shared state, or codegen.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"from": map[string]string{"type": "string", "description": "Source symbol"},
					"to":   map[string]string{"type": "string", "description": "Target symbol"},
				},
				"required": []string{"from", "to"},
			},
		},
		{
			"name":        "codesearch_field_flow",
			"description": "Find all functions that read or write a specific struct field. Shows cross-function data flow through shared state.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"type":  map[string]string{"type": "string", "description": "Struct type, e.g. 'kernel.Kernel'"},
					"field": map[string]string{"type": "string", "description": "Field name, e.g. 'SaveRestoreExecConfig'"},
				},
				"required": []string{"type", "field"},
			},
		},
	}
}

func toolResult(data interface{}) map[string]interface{} {
	content, _ := json.Marshal(data)
	return map[string]interface{}{
		"content": []map[string]interface{}{
			{"type": "text", "text": string(content)},
		},
	}
}

func toolError(msg string) map[string]interface{} {
	return map[string]interface{}{
		"content": []map[string]interface{}{
			{"type": "text", "text": fmt.Sprintf("Error: %s", msg)},
		},
		"isError": true,
	}
}

func (s *Server) sendResult(id interface{}, result interface{}) {
	resp := jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: result}
	data, _ := json.Marshal(resp)
	fmt.Println(string(data))
}

func (s *Server) sendError(id interface{}, code int, msg string) {
	resp := jsonRPCResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
	data, _ := json.Marshal(resp)
	fmt.Println(string(data))
}
