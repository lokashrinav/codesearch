// Package mcp implements the MCP server for Claude Code integration.
// The server returns fact graph subgraphs that Claude Code reasons over.
// No LLM API calls from the server — Claude Code IS the LLM.
package mcp

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/lokashrinav/codesearch/internal/query"
	"github.com/lokashrinav/codesearch/internal/storage"
)

// Server handles MCP JSON-RPC requests over stdio.
type Server struct {
	db     *sql.DB
	reader *storage.Reader
}

// NewServer creates an MCP server backed by an indexed database.
func NewServer(reader *storage.Reader) *Server {
	return &Server{reader: reader}
}

// NewServerWithDB creates an MCP server with direct DB access for subgraph extraction.
func NewServerWithDB(db *sql.DB, reader *storage.Reader) *Server {
	return &Server{db: db, reader: reader}
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

// Run starts the MCP server over stdio.
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
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": "codesearch", "version": "0.1.0"},
		})

	case "tools/list":
		s.sendResult(req.ID, map[string]interface{}{"tools": toolDefinitions()})

	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		json.Unmarshal(req.Params, &params)
		s.sendResult(req.ID, s.callTool(params.Name, params.Arguments))

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
			Query string `json:"query"`
		}
		json.Unmarshal(args, &input)
		if s.db == nil {
			return toolError("Database not available for subgraph extraction")
		}
		subgraph, err := query.BuildSubgraph(s.db, input.Query, 25)
		if err != nil {
			return toolError(err.Error())
		}
		return toolResult(subgraph)

	case "codesearch_trace":
		var input struct {
			Symbol string `json:"symbol"`
		}
		json.Unmarshal(args, &input)
		idents, err := s.reader.FindByName(input.Symbol)
		if err != nil || len(idents) == 0 {
			return toolError(fmt.Sprintf("Symbol not found: %s", input.Symbol))
		}

		var result strings.Builder
		for _, id := range idents {
			result.WriteString(fmt.Sprintf("[%d] %s (%s) at %s:%d\n", id.Kind, id.Name, id.PkgPath, id.File, id.Line))

			callers, _ := s.reader.GetCallers(id.ID)
			for _, c := range callers {
				result.WriteString(fmt.Sprintf("  <- called by %d\n", c.CallerID))
			}
			callees, _ := s.reader.GetCallees(id.ID)
			for _, c := range callees {
				result.WriteString(fmt.Sprintf("  -> calls %d\n", c.CalleeID))
			}
		}
		return toolResult(result.String())

	case "codesearch_field_flow":
		var input struct {
			Type  string `json:"type"`
			Field string `json:"field"`
		}
		json.Unmarshal(args, &input)
		writers, _ := s.reader.GetFieldWriters(input.Type, input.Field)
		readers, _ := s.reader.GetFieldReaders(input.Type, input.Field)

		var result strings.Builder
		result.WriteString(fmt.Sprintf("## Field flow: %s.%s\n\n", input.Type, input.Field))
		result.WriteString("### Writers:\n")
		for _, w := range writers {
			result.WriteString(fmt.Sprintf("  %s:%d (func %d)\n", w.File, w.Line, w.FuncID))
		}
		result.WriteString("\n### Readers:\n")
		for _, r := range readers {
			result.WriteString(fmt.Sprintf("  %s:%d (func %d)\n", r.File, r.Line, r.FuncID))
		}
		return toolResult(result.String())

	case "codesearch_explain":
		var input struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		json.Unmarshal(args, &input)

		fromIdents, _ := s.reader.FindByName(input.From)
		toIdents, _ := s.reader.FindByName(input.To)
		if len(fromIdents) == 0 {
			return toolError(fmt.Sprintf("Source not found: %s", input.From))
		}
		if len(toIdents) == 0 {
			return toolError(fmt.Sprintf("Target not found: %s", input.To))
		}

		walkResults, _ := s.reader.Walk([]uint64{fromIdents[0].ID}, 5, 500)
		var result strings.Builder
		result.WriteString(fmt.Sprintf("## Path: %s -> %s\n\n", input.From, input.To))
		targetID := toIdents[0].ID
		for _, wr := range walkResults {
			result.WriteString(fmt.Sprintf("  [hop %d] %s (%s) at %s:%d via %s\n",
				wr.Depth, wr.Ident.Name, wr.Ident.PkgPath, wr.Ident.File, wr.Ident.Line, wr.EdgeKind))
			if wr.Ident.ID == targetID {
				result.WriteString("  *** TARGET REACHED ***\n")
				break
			}
		}
		return toolResult(result.String())

	default:
		return toolError(fmt.Sprintf("Unknown tool: %s", name))
	}
}

func toolDefinitions() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"name":        "codesearch_search",
			"description": "Search for code mechanisms from a symptom description. Returns a subgraph of relevant symbols with their relationships. You (Claude) should trace the causal chain through the subgraph to explain the root cause.",
			"inputSchema": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"query": map[string]string{"type": "string", "description": "Natural language problem description"}},
				"required":   []string{"query"},
			},
		},
		{
			"name":        "codesearch_trace",
			"description": "Look up a specific symbol and its callers/callees in the code graph.",
			"inputSchema": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"symbol": map[string]string{"type": "string", "description": "Symbol name to look up"}},
				"required":   []string{"symbol"},
			},
		},
		{
			"name":        "codesearch_explain",
			"description": "Find the connection path between two symbols through the code graph.",
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
			"description": "Find all functions that read or write a specific struct field.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"type":  map[string]string{"type": "string", "description": "Struct type name"},
					"field": map[string]string{"type": "string", "description": "Field name"},
				},
				"required": []string{"type", "field"},
			},
		},
	}
}

func toolResult(data interface{}) map[string]interface{} {
	text := ""
	switch v := data.(type) {
	case string:
		text = v
	default:
		b, _ := json.Marshal(v)
		text = string(b)
	}
	return map[string]interface{}{
		"content": []map[string]interface{}{{"type": "text", "text": text}},
	}
}

func toolError(msg string) map[string]interface{} {
	return map[string]interface{}{
		"content": []map[string]interface{}{{"type": "text", "text": "Error: " + msg}},
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
