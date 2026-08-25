# Especificação para novo projeto — MCP de troubleshooting Kubernetes (via IAM Role/STS)

**Propósito deste documento:** contexto completo para o Kiro construir um projeto novo, do zero, com as features da empresa — não é um pedido pra continuar o `k8s-ts-mcp` existente. O `k8s-ts-mcp` (`github.com/chwiee/k8s-ts-mcp`, privado) foi um protótipo/prova de conceito; este documento descreve **só a parte validada e relevante pra esse novo projeto**: o caminho via IAM Role/STS, sem gRPC, e só execução de comando não destrutiva. Onde for útil, referencio arquivos do protótipo como exemplo de implementação — não como código a copiar literalmente, já que o novo projeto vai integrar com sistemas reais da empresa (a começar pelo GoldenBridge) que o protótipo só simulava.

## 1. O que é

Um MCP (Model Context Protocol) server de troubleshooting de Kubernetes para a frota de clusters da empresa (1000+). Um agente de LLM (ex: um assistente interno) chama as tools deste MCP pra diagnosticar problemas em clusters — nunca o usuário final interagindo direto com kubectl.

## 2. Arquitetura: só via IAM Role/STS, sem agente por cluster

**Decisão do time**: não usar um agente (gRPC ou qualquer outro) rodando dentro de cada cluster. O hub central assume uma **IAM Role via STS** e fala direto com a API do EKS de cada cluster.

Fluxo, dado um `cluster_id` que o usuário informou:

1. O hub resolve `cluster_id → {aws_account_id, region, eks_cluster_name}` consultando o **GoldenBridge** (ver seção 3 — é a fonte de verdade de conta/região).
2. Monta o ARN da role a assumir: `arn:aws:iam::<aws_account_id>:role/<nome-fixo-da-role>` — a role tem **nome fixo, igual em toda conta**, provisionada via módulo Terraform no momento da criação do cluster, já com as permissões corretas (o escopo exato de permissões é decisão da empresa — no protótipo era só leitura; se este projeto vai executar comandos não destrutivos como `kubectl logs`/`describe`/`get`, a role precisa desses verbs também, não só `get`/`list`).
3. Assume a role via STS (`sts:AssumeRole`), resolve endpoint/CA do cluster via `eks:DescribeCluster`, autentica na API do Kubernetes com token no formato do `aws-iam-authenticator` (renovado a cada chamada, expira em ~15min).
4. A partir daí, fala com a API do Kubernetes normalmente (client-go ou equivalente).

**Não existe cadastro por cluster** além do que o GoldenBridge já tem. Adicionar um cluster novo numa conta que a empresa já tem não deve exigir nenhuma mudança de config neste MCP — só a role já provisionada pelo Terraform da conta.

**Sem "listar todos os clusters"**: como não há um catálogo próprio, não faz sentido ter uma tool tipo `list_clusters` a menos que o GoldenBridge exponha um endpoint de listagem por escopo (conta/time). O usuário (ou o agente de LLM) sempre precisa informar o `cluster_id` explicitamente — a regra de ouro é: **nunca adivinhar qual cluster o usuário quer dizer**, mesmo que ele só tenha mencionado uma região ou conta.

## 3. GoldenBridge — fonte de verdade de conta/região (obrigatório integrar)

O **GoldenBridge** é o inventário de assets AWS da empresa (contas + recursos — EKS, EC2, ECR, SQS, LB — organizados por região, com tags, `env` e status). Ele **tem o contexto** que este MCP precisa: dado um cluster, qual conta AWS, qual região, qual ambiente (dev/uat/prod). Isso **substitui** qualquer inventário paralelo/manual — não construir um cadastro próprio de clusters neste projeto novo, consultar o GoldenBridge.

**Contrato hoje (validado, não é suposição)** — modelo de dados:

- `aws_account`: `id`, `name`, `owner`, `env` (dev/uat/prod)
- `resource`: `id` (PK interno), `account_id` (FK), `resource_type` (`eks`/`ec2`/`ecr`/`sqs`/`lb`), `resource_name`, `region`, `tags`, `env` (herdado da conta), `status` (active/deleted)

API REST: `GET /accounts`, `GET /accounts/{id}`, `GET /resources` (filtros: `account_id`, `status`, `env` — **não tem filtro por nome nem por `resource_type`**), `GET /resources/{id}` (por PK numérico, não pelo nome do recurso).

**Gap conhecido a resolver com o time do GoldenBridge**: não existe endpoint de busca por `resource_name`. Este MCP precisa resolver "dado o nome do cluster, qual conta/região" — hoje isso exigiria listar `/resources` (sem filtro) e filtrar client-side por `resource_type=eks` + `resource_name=cluster_id`, o que funciona pro volume da empresa (milhares de recursos, não milhões) mas não é o padrão ideal de uma API de lookup. **Decisão a tomar**: o GoldenBridge ganha um endpoint de busca por nome (ex: `GET /resources?resource_name=X&resource_type=eks`), ou este MCP mantém uma cache local populada por polling periódico de `/resources`. Qualquer uma das duas é aceitável pro MVP — só precisa ser decidido antes de implementar.

**Bônus**: o campo `env` de `aws_account`/`resource` já responde "esse cluster é prod?" — usar isso pro RBAC de risco (seção 4), não uma flag manual separada.

**Autenticação real**: o GoldenBridge de teste não tem autenticação nenhuma — a versão real da empresa certamente vai exigir algo (mTLS, API key, IAM SigV4, OIDC). Este é outro ponto a definir antes de apontar em produção.

## 4. RBAC — três camadas independentes

Cada uma responde uma pergunta diferente; não simplificar pra uma só.

1. **Tier por grupo AD** — "esse grupo pode disparar essa classe de ação nesse ambiente (dev/uat/prod, via `env` do GoldenBridge)?" Ex: `readonly` só diagnostica, tiers mais altos podem pedir execução.
2. **Escopo de conta AWS por agente chamador** — "o token/identidade de quem está chamando o MCP pode tocar nessa conta AWS?" Um agente de um time (ex: MARS) só deve conseguir alcançar clusters nas contas AWS daquele time, **nunca de outro time**, mesmo que peça um `cluster_id` de outra conta. Essa validação usa o resultado da consulta ao GoldenBridge (conta resolvida) — se a conta não bate com o escopo do chamador, ou se o GoldenBridge não conhece o cluster, a resposta é erro de permissão, **sem tentar nenhuma chamada AWS**. Fail-closed: token ausente ou não reconhecido nega tudo, nunca cai num default permissivo.
3. **Visibilidade de tool por grupo** — algumas tools (se houver alguma sensível, mesmo sendo não destrutiva — ex: coleta de logs pode expor dado sensível) podem precisar ser restritas a determinados grupos. Um chamador sem o grupo certo não deve nem saber que a tool existe (não aparece na listagem de tools do MCP), não só ser bloqueado ao tentar chamar.

**Identidade real**: a integração de identidade (SSO/OIDC/AD real) do agente de LLM chamador ainda não está definida — é um ponto em aberto, não bloqueante pra começar a construir (dá pra simular com tokens fixos durante o desenvolvimento), mas precisa ser resolvido antes de produção.

## 5. Features do MVP (execução só não destrutiva)

**Importante**: este MVP não executa nenhuma ação que modifica o cluster (sem restart de pod, sem scale, sem rollout restart, sem delete). Tudo que seria uma remediação real fica só como **proposta de comando** pro usuário rodar manualmente — igual um "modo dry-run permanente". As únicas execuções reais permitidas são leituras: `get`, `describe`, `logs`, equivalentes. Isso é uma escolha deliberada de escopo pro MVP, não uma limitação técnica — dá pra evoluir depois.

| Feature | Descrição | Por que é MVP |
|---|---|---|
| **Diagnóstico de sinal** (`troubleshoot`-like) | Dado `cluster_id` + tipo de problema (ex: pod em CrashLoopBackOff) + namespace/nome, identifica a causa e **propõe** os comandos de correção — nunca executa nada que mude estado. | É o valor central do produto: dizer o que está errado e o que fazer, mesmo sem executar. |
| **Scan de cluster** | Lista pods/recursos com problema num cluster (ou namespace), sem precisar que o usuário já saiba o nome exato — descoberta antes do diagnóstico. | Sem isso o usuário precisa saber de antemão o que está quebrado, o que não é realista. |
| **Coleta de logs por namespace** | **Obrigatório, ver seção 6.** | Requisito explícito do time. |
| **Listagem de nodes** | Nome, zona, região, tipo de instância, arquitetura, status Ready — com contexto de conta/região vindo do GoldenBridge. | Pergunta operacional comum ("quais nodes estão em tal região"), read-only. |
| **Post-mortem / histórico de incidente** | Texto simples resumindo o que foi diagnosticado e proposto, a partir de uma trilha de auditoria própria. | Rastreabilidade — mesmo sem executar nada, o diagnóstico precisa ficar registrado. |
| **RBAC completo (seção 4)** | Tier + escopo de conta + visibilidade de tool. | Sem isso, qualquer chamador alcança qualquer cluster de qualquer conta — inaceitável numa empresa com múltiplos times/contas. |
| **Redação de segredos na origem** | Nenhum dado de `Secret`, token, credencial é lido ou aparece em log/output/auditoria — a redação acontece antes do dado sair do ponto de coleta, não como filtro depois. | Requisito de segurança não-negociável, mesmo em modo só-leitura (logs de aplicação podem vazar segredo sem querer). |
| **Trilha de auditoria imutável** | Todo diagnóstico/consulta fica registrado (quem pediu, quando, o que foi encontrado, o que foi proposto). | Rastreabilidade e compliance. |

## 6. Requisito obrigatório: coleta de logs por namespace

Diferente de "logs de um pod específico" (que normalmente já existe em qualquer ferramenta), este requisito é **coletar logs de todos os pods de um namespace**, sob demanda, quando o usuário pedir — ex: "me mostra os logs do namespace `checkout` das últimas 2h" ou "tem algum erro nos logs do namespace `payments` agora?".

Critérios de aceite (formato de requisito):

- **QUANDO** o usuário/agente solicitar logs de um namespace (`cluster_id` + `namespace`, sem precisar informar pod específico), **O SISTEMA DEVE** coletar os logs de todos os pods daquele namespace (ou de um subconjunto filtrável — ex: por label/deployment — a definir) e retornar de forma agregada.
- **QUANDO** o namespace tiver muitos pods, **O SISTEMA DEVE** ter algum limite/paginação sensato (não travar nem estourar memória tentando trazer logs de centenas de pods de uma vez) — o comportamento exato (truncar por pod, limitar quantidade de pods, janela de tempo obrigatória) fica a definir.
- **QUANDO** um pod específico não puder ter seus logs lidos (ex: sem permissão, contêiner ainda não iniciado), **O SISTEMA DEVE** reportar isso pontualmente sem falhar a coleta inteira dos outros pods do namespace.
- Essa coleta é **sempre não destrutiva** — só leitura (`kubectl logs` equivalente), nunca aciona nada.
- A mesma redação de segredos (seção 5) se aplica aqui — nenhum segredo pode vazar via log agregado.

Este é o único item que o time marcou como obrigatório desde já; o resto da superfície de logs (ex: correlação com métricas, retenção, streaming em tempo real) fica em aberto pra discussão.

## 7. Fora do escopo deste MVP (discutir depois)

- **Qualquer execução que modifica o cluster** — restart de pod, scale, rollout restart, aumento de limite de recursos, etc. Tudo isso, se vier a existir, precisa de um desenho de risco/aprovação separado (o protótipo `k8s-ts-mcp` tem um modelo de escada de ações + aprovação explícita pra ações de alto risco, que pode servir de referência — ver `internal/execengine` e o fluxo `approve_action` em `docs/ARCHITECTURE.md` daquele repo — mas isso é uma decisão de produto pra depois, não pro MVP).
- **Agente rodando dentro do cluster (gRPC ou qualquer outro protocolo)** — decisão do time de não usar esse modelo por ora.
- **Identidade real (SSO/OIDC)** do chamador — MVP pode simular com tokens fixos.
- **Catálogo de runbooks / conhecimento de referência pra sinais sem diagnóstico automatizado** — útil, mas não bloqueante pro MVP funcionar.
- **Multi-réplica do hub / alta disponibilidade** — como não há agente persistente por cluster nesse modelo (é tudo via STS sob demanda), isso é mais simples que no protótipo original e pode ficar pra quando o volume de uso justificar.

## 8. Referência

O protótipo `k8s-ts-mcp` (`github.com/chwiee/k8s-ts-mcp`) tem implementação de referência (não pra copiar direto, já que usa mocks locais em vez de integrações reais da empresa) pra:
- Resolução de cluster por convenção via IAM Role: `internal/rolecluster`
- Cliente Kubernetes via STS: `internal/k8sclient/awsrole.go`
- As 3 camadas de RBAC: `internal/policy`, `internal/agentauth`, `internal/mcptools/toolaccess.go`
- Tools MCP e formato de resposta: `internal/mcptools/mcptools.go`
- Documento de gap do GoldenBridge (mais detalhe que a seção 3 acima): `docs/inventory-api-decision.md`
