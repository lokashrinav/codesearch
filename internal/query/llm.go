package query

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// LLMConfig configures the Claude API connection.
type LLMConfig struct {
	APIKey string
	Model  string // default: claude-sonnet-4-6
}

// LLMTrace asks Claude to trace a causal chain through a code subgraph.
func LLMTrace(config LLMConfig, symptom string, graphContext string) (string, error) {
	if config.APIKey == "" {
		return "", fmt.Errorf("no API key configured")
	}
	model := config.Model
	if model == "" {
		model = "claude-sonnet-4-6"
	}

	prompt := fmt.Sprintf(`You are a code comprehension engine analyzing a Go codebase.

Developer's problem: %q

Below is a subgraph of relevant code symbols with their relationships (-> outgoing edges, <- incoming edges):

%s

Trace the causal chain that explains this symptom. For each step:
1. Name the specific symbol and file:line
2. Explain WHY it's relevant to the symptom
3. Explain HOW it connects to the next step

Focus on mechanisms that could cause the symptom silently (nil checks, missing initialization, conditional paths). Be specific about file locations.`, symptom, graphContext)

	body := map[string]interface{}{
		"model":      model,
		"max_tokens": 1500,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	}

	jsonBody, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(jsonBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", config.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody[:min(500, len(respBody))]))
	}

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	if len(result.Content) == 0 {
		return "", fmt.Errorf("empty response from API")
	}

	return result.Content[0].Text, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
