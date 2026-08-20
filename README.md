# k8s-ts-mcp

MCP de troubleshooting preditivo de Kubernetes, rodando em um cluster hub central e atendendo uma frota de 1000+ clusters via um agente leve por cluster. Veja [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) para o desenho completo.

## Getting started

```bash
go run ./cmd/hub-server
go run ./cmd/cluster-agent
```

## Project structure

- `cmd/hub-server/` — MCP server central: tools MCP, motor de troubleshooting, policy engine, orquestração via gRPC
- `cmd/cluster-agent/` — agente que roda em cada cluster remoto: conexão outbound mTLS pro hub, execução local de playbooks
- `internal/playbooks/` — interface de plugin de diagnóstico/remediação (core k8s, Calico, KEDA, ...)
- `internal/execengine/` — escada de escalonamento (até 3 ações), snapshot/rollback
- `internal/postmortem/` — geração de post-mortem em texto simples a partir do audit trail
- `internal/audit/` — trilha de auditoria imutável
- `internal/transport/` — canal gRPC bidirecional mTLS hub↔agente
- `internal/policy/` — flag global de execução + RBAC via grupos AD/OIDC (OPA)
- `pkg/` — código exposto para uso externo
- `api/` — contratos gRPC/proto entre hub e agente
- `deployments/` — manifests k8s e ApplicationSets do ArgoCD (hub-server, cluster-agent, playbook bundles)
- `docs/` — arquitetura e decisões de design

## Development

```bash
make build   # compila os dois binários
make test    # roda os testes
make run-hub-server
make run-cluster-agent
```
