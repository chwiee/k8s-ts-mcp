# Handoff — k8s-ts-mcp

**Escrito em:** 2026-08-25, ao final de uma sessão de trabalho intensa. Este documento existe pra qualquer pessoa (ou IA) que assuma o projeto sem contexto prévio conseguir entender o estado real, retomar o ambiente de teste e saber exatamente o que falta. Leia isto antes de `docs/ARCHITECTURE.md` — aquele é o desenho técnico; este é "onde paramos e por quê".

## O que é este projeto

MCP de troubleshooting preditivo de Kubernetes para 1000+ clusters da empresa. Repo: `github.com/chwiee/k8s-ts-mcp` (privado). Local: `C:\wb\golang\k8s-ts-mcp`. Linguagem: Go. SDK MCP: `github.com/modelcontextprotocol/go-sdk`.

Modelo original era hub-and-spoke com um `cluster-agent` gRPC discando de dentro de cada cluster. **Isso está desabilitado por decisão do time (2026-08-25) — só se usa o caminho via IAM Role/STS agora.** Ver seção "Decisões operacionais" abaixo antes de assumir qualquer coisa sobre gRPC.

## Como o repo está estruturado (mapa de pacotes)

```
cmd/hub-server/       — processo central: MCP Streamable HTTP (:8443) + gRPC pra agentes (:7443, hoje sem uso)
cmd/cluster-agent/    — agente por cluster via gRPC — DESLIGADO, não rodar por ora
cmd/smoketest/        — cliente MCP de linha de comando pra testar as tools na mão (tem --token pra simular chamador)

internal/mcptools/    — as 5 (4 registradas) tools MCP + checkClusterScope (RBAC de conta) + ToolAccess (RBAC de visibilidade)
internal/policy/      — RBAC de tier (AD-group -> readonly/nonprod-admin/prod-admin), OPA/Rego embutido
internal/agentauth/   — token do agente chamador -> {contas AWS permitidas, grupos AD simulados} — ver "RBAC" abaixo
internal/inventory/   — cluster_id -> {aws_account_id, region, eks_cluster_name}, via YAML (dev) ou HTTPClient (contrato ASSUMIDO, não confirmado)
internal/rolecluster/ — resolve cluster_id -> Handler via IAM Role, por convenção de nome (sem cadastro por cluster)
internal/agentcore/   — implementa transport.Handler (Diagnose/Execute/Scan/ApproveAction/GetLogs/ListNodes) sobre client-go
internal/k8sclient/   — único ponto que fala com a API do Kubernetes (client-go) + NewFromAWSRole (STS)
internal/execengine/  — escada de escalonamento (até 3 ações), snapshot/rollback, RunApproved p/ ação de risco alto
internal/playbooks/   — corek8s (CrashLoopBackOff, OOMKilled, ImagePullBackOff, ExecFormatError), calico, keda
internal/transport/   — canal gRPC mTLS hub<->agente (hoje sem uso) + forwarding entre réplicas (Redis, hoje sem uso)
internal/registry/    — registro Redis/in-memory de qual réplica tem qual conexão de agente — SÓ RELEVANTE COM gRPC ATIVO
internal/audit/       — trilha de auditoria imutável (FileStore, JSONL)
internal/postmortem/  — gera texto de post-mortem a partir do audit trail
internal/runbooks/    — catálogo Markdown de fallback (TF-IDF, log_signatures) quando não há playbook compilado
internal/redact/      — scrubber de segredos, aplicado na origem (agent-side)
internal/tlsutil/     — credenciais mTLS — SÓ RELEVANTE COM gRPC ATIVO

api/proto/k8sts/v1/   — contrato gRPC (codegen via `buf generate`, precisa buf+protoc-gen-go+protoc-gen-go-grpc no PATH)
docs/ARCHITECTURE.md  — desenho técnico completo, decisões e "porquês"
docs/inventory-api-decision.md — gap concreto da API de inventário real (ver abaixo)
docs/runbooks/kubernetes-errors.md — catálogo de runbooks
deployments/          — manifests ArgoCD/K8s (hub-server, cluster-agent, group-mapping.example.yaml)
```

## RBAC — 3 camadas independentes, todas implementadas hoje

Não confundir as três — cada uma responde uma pergunta diferente:

1. **`internal/policy`** — "esse grupo AD pode executar essa ação de risco X nesse ambiente (prod/nonprod)?" Tier: readonly < nonprod-admin < prod-admin. Risco alto sempre exige `approve_action` manual, nunca roda sozinho mesmo com flag de execução ligada.
2. **`internal/agentauth`** (escopo de conta) — "esse token de agente chamador pode tocar nessa conta AWS?" Config pequena e estável (um agente/time, não um cluster). Falha fechado: token ausente/desconhecido quando `--agent-scopes-config` está ativo = nega tudo (`agentauth.DenyAllScope`), nunca cai no default permissivo (`nil`, reservado só pra "sem config nenhuma").
3. **`internal/mcptools.ToolAccess`** (visibilidade de tool) — "esse grupo AD pode nem saber que essa tool existe?" Substring de nome de grupo (ex: precisa conter "SRE"). Enforced em `Register()` — a tool nem é registrada no `*mcp.Server` daquela sessão, então `tools/call` retorna `unknown tool`, não um erro de permissão.

**Checagem central de escopo por cluster**: `mcptools.Server.checkClusterScope(ctx, clusterID)` — chamada por `scan_cluster`, `troubleshoot`, `approve_action` e `list_nodes` (não por `get_postmortem`, que só lê auditoria local, nunca toca AWS). Resolve `cluster_id` via `Inventory`, confere `AgentScope.AllowsAccount`. Se `AgentScope != nil` e o cluster não está no inventário: **nega** (não dá pra provar escopo, falha fechado) — mesmo que o cluster exista de verdade.

**Identidade por sessão, não global**: o SDK MCP cria um `*mcp.Server` novo por sessão (`NewStreamableHTTPHandler`'s `getServer` é chamado uma vez por sessão, com acesso ao `*http.Request` inicial). `cmd/hub-server/main.go`'s `newSessionServer` extrai o Bearer token do header `Authorization`, resolve `agentauth.Scope` (conta + `ADGroups`), e constrói uma cópia rasa de `mcptools.Server` com `AgentScope`/`CallerGroups` daquela sessão — `Hub`/`Policy`/`Audit`/`Runbooks`/`Inventory` continuam compartilhados (ponteiros).

**IMPORTANTE — identidade real ainda não existe.** `agentauth.AgentConfig.ADGroups` é uma **simulação** de AD/SSO (ver comentário de pacote em `internal/agentauth/agentauth.go`) — não é OIDC/SSO real. Sem `--agent-scopes-config`, cai no stub antigo (`--test-caller-groups`, global, um valor só pra toda sessão). Isso é o maior gap pra produção: RBAC de tier e de visibilidade de tool são reais no código, mas a identidade que alimenta eles ainda é fingida.

## Descoberta de cluster — por convenção, sem cadastro

`internal/rolecluster.Manager.Handler(ctx, clusterID)`:
1. Consulta `Inventory.Lookup(ctx, clusterID)` → `{aws_account_id, region, eks_cluster_name}`.
2. Monta `arn:aws:iam::<account_id>:role/k8s-ts-mcp-readonly` — **nome de role fixo**, criado via módulo Terraform em cada conta (decisão do time, já read-only).
3. Assume a role via STS, chama `eks:DescribeCluster`, conecta.

**Consequência importante que já foi testada e confirmada**: uma vez que um `cluster_id` está no `Inventory`, ele fica **travado** nesse caminho — `transport.Server.tryRole` nunca cai pro gRPC-agent depois que `found=true`, mesmo que a chamada à AWS falhe. Como gRPC está desligado mesmo, isso não importa na prática agora, mas é bom saber: **não dá pra ter um cluster "híbrido"** (às vezes por Role, às vezes por agente) só por estar ou não no inventário.

`list_clusters` está **desabilitada** (não registrada, mas o handler continua no código) — sem cadastro, não existe "lista de todos os clusters", só "esse cluster_id existe?". Reativar quando a API real de inventário tiver endpoint de listagem por escopo.

## Decisões operacionais (2026-08-25, confirmadas pelo time)

| Decisão | Consequência prática |
|---|---|
| gRPC/`cluster-agent` desligado por ora (talvez volte no futuro) | Não rodar `cmd/cluster-agent`. `internal/registry` (Redis multi-réplica) e `internal/tlsutil` (mTLS) ficam sem função nesse modo — não removidos do código, só não usados. |
| Só se usa IAM Role/STS | `internal/rolecluster` é o único caminho ativo pra alcançar um cluster. |
| TLS do endpoint MCP: ELB/ALB no EKS, não o processo Go | `hub-server` continua servindo HTTP puro internamente. Falta ainda confirmar sticky session por cookie nesse ELB (ver `docs/ARCHITECTURE.md`, seção "Registro compartilhado..."). |
| `--execution-enabled` desligado por padrão | Mitigação temporária pro bug de falso-positivo abaixo — reavaliar quando corrigido. |

## Bug conhecido, NÃO corrigido (prioridade alta pra corrigir antes de religar execução)

**`internal/playbooks/corek8s/crashloopbackoff.go:94-103`** (ação "rollout restart deployment") e **`internal/playbooks/corek8s/oomkilled.go`'s `Recheck`** (mesmo padrão) validam checando `DeploymentReadyReplicas >= want` **uma vez** via `waitFor` (polling que para no primeiro `true`). Sem `readinessProbe` no pod, um container novo conta como "Ready" no instante em que sobe — antes de crashar de novo, o que é exatamente a definição de CrashLoopBackOff. Resultado: `troubleshoot` pode reportar `resolved: true` (ou `Recheck` pode liberar uma aprovação) quando o pod **continua quebrado**.

**Reproduzido ao vivo em 2026-08-25** contra Floci: `troubleshoot` no `core/crashloopbackoff` reportou `resolved: true` depois do `rollout restart`, mas `kubectl get pods` mostrou o pod ainda em `Error`/restart subindo, segundos depois.

**Correção sugerida (não implementada ainda)**: `Validate` precisa confirmar estabilidade numa janela de tempo (ex: `ReadyReplicas >= want` sustentado por N segundos sem novo restart), não uma leitura instantânea. Mesma classe de bug que já foi corrigida uma vez no projeto pro OOMKilled original (ver `docs/ARCHITECTURE.md`), só que reapareceu numa ladder diferente.

**Mitigação atual**: `--execution-enabled=false` por padrão — sem execução real, o bug não pode causar dano (só afeta `dry_run=false`, que não acontece).

## Gaps abertos, em ordem de prioridade pra produção

1. **Bug de validação acima** — corrigir antes de religar `--execution-enabled`.
2. **Identidade real do chamador (SSO/OIDC)** — `agentauth`'s `ADGroups` é simulação, não integração real. Ver `internal/agentauth/agentauth.go`'s comentário de pacote.
3. **Contrato real da API de inventário** — ver `docs/inventory-api-decision.md`. `goldenbridge` (`C:\wb\golang\goldenbridge`, mock local rodando em `:8080`) tem modelo de dados real (contas/recursos AWS com `env`), mas **não tem endpoint de busca por nome de recurso** — `internal/inventory/httpclient.go`'s contrato assumido (`GET /clusters/{id}`) não bate nem com o mock. Precisa decidir com o time: goldenbridge vira a API real, ou é outra coisa?
4. **`ClusterEnv` (prod/nonprod)** ainda via flag CSV manual (`--prod-clusters`) — `goldenbridge` já tem campo `env` que poderia substituir isso.
5. **Emissão/rotação de certificado mTLS** — não relevante enquanto gRPC estiver desligado (ver acima), mas se ele voltar, ainda não existe.
6. **Sticky session por cookie no ELB real** — só testado localmente com `sessionAffinity: ClientIP` como stand-in.

## Ambiente de teste local — como reconstruir do zero

Se o ambiente cair (aconteceu nesta sessão — o container `floci-test` tinha sumido inteiro após reinício da máquina), os passos pra reconstruir:

```bash
# 1. Floci (emulador AWS local) — precisa do socket do Docker montado
docker run -d --name floci-test -p 4566:4566 -v //var/run/docker.sock:/var/run/docker.sock floci/floci:latest

# 2. Credenciais fake + endpoint, pra qualquer comando aws/go que siga
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_ENDPOINT_URL=http://localhost:4566 AWS_REGION=us-east-1

# 3. Role fixa por convenção (nome exato que internal/rolecluster monta)
aws iam create-role --role-name k8s-ts-mcp-readonly \
  --assume-role-policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"*"},"Action":"sts:AssumeRole"}]}'

# 4. Cluster EKS emulado (Floci sobe um container k3s real por trás)
#    subnets default já existem no Floci — confira com `aws ec2 describe-subnets`
aws eks create-cluster --name probe-cluster \
  --role-arn arn:aws:iam::000000000000:role/k8s-ts-mcp-readonly \
  --resources-vpc-config subnetIds=subnet-default-a,subnet-default-b

# 5. Esperar ACTIVE (10-30s)
aws eks describe-cluster --name probe-cluster --query 'cluster.status'
```

Isso recria exatamente o cenário `spoke-role-1` que os `.scratch-*.yaml` já esperam (conta `000000000000`, região `us-east-1`, `eks_cluster_name: probe-cluster`).

**Kind (`kind-spoke-1`)** ainda existe como container Docker (`spoke-1-control-plane`), mas **não é mais usado** — era o cluster de teste do caminho gRPC, que está desligado. Não precisa recriar a menos que gRPC volte a ser testado.

**GoldenBridge** (mock de API de inventário real, não usado pelo `hub-server` ainda — só documentado): `docker start goldenbridge` se tiver parado, ou `docker run -d --name goldenbridge -p 8080:8080 -v goldenbridge-data:/data goldenbridge:latest` do zero (fonte em `C:\wb\golang\goldenbridge`).

### Subindo o hub-server

```bash
cd C:\wb\golang\k8s-ts-mcp
go build -o bin/hub-server ./cmd/hub-server
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_ENDPOINT_URL=http://localhost:4566 AWS_REGION=us-east-1
./bin/hub-server --insecure \
  --cluster-inventory-path=.scratch-cluster-inventory.yaml \
  --agent-scopes-config=.scratch-agent-scopes.yaml \
  --tool-access-config=.scratch-tool-access.yaml \
  --group-mapping=.scratch-group-mapping.yaml \
  --runbooks-path=docs/runbooks/kubernetes-errors.md
# --execution-enabled=true SÓ depois de corrigir o bug de validação acima
```

Não precisa mais de `--role-clusters-config` (não existe mais essa flag) nem de rodar `cmd/cluster-agent`.

### Arquivos `.scratch-*.yaml` (gitignored, recriar se sumirem)

```yaml
# .scratch-cluster-inventory.yaml
clusters:
  - cluster_id: spoke-role-1
    aws_account_id: "000000000000"
    region: us-east-1
    eks_cluster_name: probe-cluster
```

```yaml
# .scratch-agent-scopes.yaml
agents:
  - name: floci-agent
    token: floci-test-token
    allowed_accounts: ["000000000000"]
    ad_groups: ["infra-sre-prod"]
  - name: mars-agent
    token: mars-test-token
    allowed_accounts: ["123456789012"]
    ad_groups: ["mars-team-readonly"]
```

```yaml
# .scratch-tool-access.yaml
tools:
  - name: approve_action
    requires_group_name: SRE
```

```yaml
# .scratch-group-mapping.yaml
groups:
  infra-sre-prod: prod-admin
  mars-team-readonly: readonly
```

### Testando

```bash
# Cenário quebrado real (CrashLoopBackOff + ImagePullBackOff)
export KUBECONFIG=/tmp/floci-k3s.yaml   # extrair: ver comando abaixo se o arquivo não existir
kubectl apply -f .scratch-broken-pods.yaml

# Extrair kubeconfig do k3s dentro do container Floci (path com barra precisa de MSYS_NO_PATHCONV no Git Bash)
MSYS_NO_PATHCONV=1 docker exec floci-eks-probe-cluster cat /etc/rancher/k3s/k3s.yaml > /tmp/floci-k3s.yaml
sed -i 's/127.0.0.1:6443/localhost:6500/' /tmp/floci-k3s.yaml

# Tools via smoketest (--token simula o chamador)
go run ./cmd/smoketest --token=floci-test-token --tool scan_cluster --args '{"cluster_id":"spoke-role-1","namespace":"default"}'
go run ./cmd/smoketest --token=floci-test-token --tool troubleshoot --args '{"cluster_id":"spoke-role-1","kind":"PodCrashLoopBackOff","namespace":"default","name":"<nome-do-pod-atual>"}'
go run ./cmd/smoketest --token=floci-test-token --tool get_postmortem --args '{"incident_id":"<id-retornado>"}'
go run ./cmd/smoketest --token=mars-test-token --tool list_nodes --args '{"cluster_id":"spoke-role-1"}'   # deve negar (conta errada)
```

**Cuidado**: nomes de pod mudam a cada `troubleshoot` que roda `restart pod` — sempre rode `scan_cluster` de novo antes de reusar um nome de pod. E processos do lado do hub continuam rodando mesmo se você matar o `smoketest` do lado do cliente (Ctrl+C/timeout local não cancela a operação no servidor).

## Estado do git

`origin/main` está **2 commits atrás** do local (`1df4ad5`, `c8a85a5`) — `git push` está travando aqui por causa de um prompt interativo do Git Credential Manager que não consegui completar (sem acesso a UI). Rodar `git push` manualmente num terminal interativo deve resolver. Verificar `git status`/`git log --oneline -5` antes de continuar trabalhando, pra não perder o rastro de onde o remoto realmente está.

## Por onde continuar

Ordem sugerida, mas é julgamento de quem retomar:

1. Corrigir o bug de validação (`crashloopbackoff.go`/`oomkilled.go` `Recheck`) — é o item que mais preocupa, porque mente sobre o resultado de uma ação real.
2. Decidir com o time a API de inventário real (`docs/inventory-api-decision.md`) — desbloqueia integração de verdade em vez de mock/YAML.
3. Decidir identidade real (SSO/OIDC) pra substituir a simulação do `agentauth`.
4. Só depois disso, religar `--execution-enabled` em produção.
