# Decisão pendente: API de inventário de clusters

**Status: em aberto — precisa ser definido antes de apontar `k8s-ts-mcp` para o ambiente real da empresa.**

## Contexto

`internal/rolecluster` e `internal/mcptools` (RBAC de escopo de conta) dependem de resolver `cluster_id → {aws_account_id, region, eks_cluster_name}` via `internal/inventory.Lookup`. Hoje isso é satisfeito de duas formas:

1. `internal/inventory.Inventory` (YAML estático) — usado nos testes locais, ver `.scratch-cluster-inventory.yaml`.
2. `internal/inventory.HTTPClient` — contrato **assumido**, nunca confirmado com um time dono de API real:
   ```
   GET {BaseURL}/clusters/{cluster_id}
   200 -> {"aws_account_id", "region", "eks_cluster_name"}
   404 -> cluster não existe
   ```

## GoldenBridge — mock local, não a API real da empresa

`goldenbridge` (`C:\wb\golang\goldenbridge`, container local `goldenbridge:latest` porta 8080) é um inventário de assets AWS real (contas + recursos EKS/EC2/ECR/SQS/LB, SQLite) que criamos como **mock local** — não é a API que existe (ou vai existir) na empresa. Serve pra prototipar/testar a integração, não pra apontar em produção sem revisão.

Contrato real do goldenbridge hoje (confirmado no código-fonte, não suposição):

| método | rota | filtros suportados |
|---|---|---|
| GET | `/accounts` | — |
| GET | `/accounts/{id}` | busca por ID exato da conta |
| GET | `/resources` | `?account_id=`, `?status=`, `?env=` (**não** tem filtro por nome nem por `resource_type`) |
| GET | `/resources/{id}` | busca por PK numérico interno, não pelo nome do recurso |

**Gap concreto**: `k8s-ts-mcp` precisa resolver "dado o nome do cluster, qual a conta/região" — mas o goldenbridge não tem endpoint de busca por `resource_name`. As opções hoje são:
- listar `/resources?account_id=X` já sabendo a conta (não serve — é justamente o que queremos descobrir);
- listar `/resources` inteiro (sem filtro) e filtrar client-side por `resource_type=eks` + `resource_name=cluster_id` — funciona pra volume baixo/médio (a escala de 1000+ clusters da empresa ainda é tranquila pra uma listagem completa), mas não é o padrão de uma API de lookup.

**Bônus não previsto**: `resource.env` (herdado de `aws_account.env`: dev/uat/prod) responde exatamente à pergunta que hoje `--prod-clusters` (flag CSV manual) resolve de forma manual em `internal/policy` — se a API real tiver campo equivalente, dá pra aposentar esse flag e ler o ambiente do próprio inventário.

## O que precisa ser definido com o time antes de ir pra produção

1. **Qual é a API real de inventário da empresa?** É o goldenbridge (produtizado), outra coisa já existente, ou algo a construir? — isso decide se `internal/inventory.HTTPClient` deve mirar o contrato do goldenbridge ou outro.
2. **Como fica o lookup por nome?** Se a API real seguir o padrão do goldenbridge (sem busca por nome), `HTTPClient` precisa implementar list+filter client-side (com cache, dado o volume de 1000+ clusters) em vez de um GET direto por ID — mudança de implementação, não só de URL.
3. **Confirmar se `env` (ou equivalente) está disponível** — se sim, dá pra eliminar `--prod-clusters` como flag manual (ver `docs/ARCHITECTURE.md`, gap "ClusterEnv").
4. **Autenticação da API real** — goldenbridge não tem auth nenhuma hoje (mock local); a API real da empresa certamente vai exigir algo (mTLS, API key, IAM SigV4, OIDC) que `HTTPClient` ainda não implementa.

Até isso ser decidido, `HTTPClient` continua com o contrato assumido documentado no próprio arquivo (`internal/inventory/httpclient.go`), e os testes usam o YAML estático. Não apontar `--inventory-api-url` para nada em produção sem revisar este documento primeiro.
