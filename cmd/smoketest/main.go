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

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	endpoint := flag.String("endpoint", "http://localhost:8443", "endereço MCP (Streamable HTTP) do hub-server")
	tool := flag.String("tool", "list_clusters", "nome da tool a chamar (list_clusters, troubleshoot, get_postmortem)")
	args := flag.String("args", "{}", "argumentos da tool, como JSON")
	flag.Parse()

	var arguments map[string]any
	if err := json.Unmarshal([]byte(*args), &arguments); err != nil {
		log.Fatalf("smoketest: parseando --args: %v", err)
	}

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "smoketest", Version: "0.0.1"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: *endpoint}, nil)
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
