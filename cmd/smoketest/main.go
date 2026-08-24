// Command smoketest is a small MCP client for calling k8s-ts-mcp's
// hub-server by hand — a "curl" for the tools in internal/mcptools, useful
// for manual testing before wiring up a real calling agent.
//
// Example:
//
//	go run ./cmd/smoketest --tool troubleshoot --args '{"cluster_id":"spoke-1","kind":"PodCrashLoopBackOff","namespace":"default","name":"crasher-xyz"}'
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// bearerRoundTripper injects "Authorization: Bearer <token>" on every
// request — how a real calling agent (see internal/agentauth) presents its
// scoped token to hub-server.
type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (rt *bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+rt.token)
	return rt.base.RoundTrip(req)
}

func main() {
	endpoint := flag.String("endpoint", "http://localhost:8443", "endereço MCP (Streamable HTTP) do hub-server")
	tool := flag.String("tool", "list_clusters", "nome da tool a chamar (list_clusters, troubleshoot, get_postmortem)")
	args := flag.String("args", "{}", "argumentos da tool, como JSON")
	token := flag.String("token", "", "token do agente chamador (ver internal/agentauth) — vazio simula um chamador sem token")
	flag.Parse()

	var arguments map[string]any
	if err := json.Unmarshal([]byte(*args), &arguments); err != nil {
		log.Fatalf("smoketest: parseando --args: %v", err)
	}

	transport := &mcp.StreamableClientTransport{Endpoint: *endpoint}
	if *token != "" {
		transport.HTTPClient = &http.Client{Transport: &bearerRoundTripper{token: *token, base: http.DefaultTransport}}
	}

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "smoketest", Version: "0.0.1"}, nil)
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		log.Fatalf("smoketest: conectando em %s: %v", *endpoint, err)
	}
	defer cs.Close()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: *tool, Arguments: arguments})
	if err != nil {
		log.Fatalf("smoketest: chamando %s: %v", *tool, err)
	}

	if res.IsError {
		fmt.Println("--- ERRO ---")
	}
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			fmt.Println(tc.Text)
		}
	}
}
