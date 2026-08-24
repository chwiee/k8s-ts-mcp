# k8s-ts-mcp

MCP de troubleshooting preditivo de Kubernetes, rodando em um cluster hub central e atendendo uma frota de 1000+ clusters via um agente leve por cluster. Veja [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) para o desenho completo.

## Getting started

Localmente, sem mTLS nem cluster real (kubeconfig aponta pro contexto atual):

```bash
go run ./cmd/hub-server --insecure
go run ./cmd/cluster-agent --insecure --cluster-id=local-test --hub-addr=localhost:7443
```

Tools MCP disponíveis (servidas via Streamable HTTP em `:8443`): `list_clusters`, `troubleshoot`, `get_postmortem` — ver `internal/mcptools`.

## Project structure

- `cmd/hub-server/` — MCP server central: tools MCP (`internal/mcptools`), gRPC (agentes), policy engine
- `cmd/cluster-agent/` — agente que roda em cada cluster remoto: conexão outbound mTLS pro hub, execução local de playbooks
- `internal/mcptools/` — tools MCP expostas pelo hub-server (`list_clusters`, `troubleshoot`, `get_postmortem`)
- `internal/agentcore/` — liga o transporte às playbooks/execengine no lado do agente
- `internal/playbooks/` — interface de plugin de diagnóstico/remediação, com `corek8s/`, `calico/`, `keda/`
- `internal/k8sclient/` — único ponto que fala com a API do Kubernetes local (client-go)
- `internal/execengine/` — escada de escalonamento (até 3 ações), snapshot/rollback
- `internal/postmortem/` — geração de post-mortem em texto simples a partir do audit trail
- `internal/audit/` — trilha de auditoria imutável
- `internal/redact/` — redação de segredos/tokens na origem, antes de sair do agente
- `internal/transport/` — canal gRPC bidirecional mTLS hub↔agente (contrato em `api/proto`), com forwarding entre réplicas do hub
- `internal/registry/` — registro compartilhado (Redis) de qual réplica do hub tem qual cluster conectado
- `internal/tlsutil/` — credenciais mTLS (com modo `--insecure` só para dev local)
- `internal/policy/` — flag global de execução + RBAC via grupos AD/OIDC (OPA embutido)
- `pkg/` — código exposto para uso externo
- `api/proto/` — contrato gRPC/proto entre hub e agente (gerado via `buf generate` para `internal/transport/gen`)
- `deployments/` — manifests k8s e ApplicationSet do ArgoCD (hub-server, cluster-agent, exemplo de group-mapping)
- `docs/` — arquitetura e decisões de design

## Development

```bash
make build   # compila os dois binários
make test    # roda os testes
make run-hub-server
make run-cluster-agent
```
