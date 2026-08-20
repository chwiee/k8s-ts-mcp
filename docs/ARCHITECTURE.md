# Arquitetura

## Requisitos

1. Roda em um cluster central (hub).
2. Atende 1000+ clusters, com troubleshooting preditivo de incidentes.
3. Flag global de execução: ligada roda a solução, desligada só retorna os comandos ao usuário.
4. Gera post-mortem em texto simples: o que causou o incidente e qual foi a solução.
5. Com permissão de execução, escada de até 3 ações; se nenhuma resolver, informa ao usuário as ações tentadas.
6. Cobre problemas gerais de Kubernetes e de recursos suportados pelo time (Calico, KEDA, ...).

## Padrão: hub-and-spoke com agente por cluster

O hub nunca guarda credencial/kubeconfig dos 1000 clusters. Cada cluster roda um `cluster-agent` que:

- Inicia conexão de saída (gRPC, mTLS) para o hub — nenhum inbound precisa ser aberto nos clusters remotos.
- Coleta sinais localmente e os envia ao hub para o motor de detecção preditiva.
- Executa os playbooks localmente quando autorizado, mais perto do recurso e sem expor a credencial do cluster ao hub.

Todos os clusters e o hub estão na mesma VPC da empresa, o que simplifica o transporte (sem relay/NAT traversal pela internet).

## RBAC — três camadas

1. **Solicitação (humano → hub):** identidade via SSO/OIDC, claims de grupo do AD mapeados por um policy engine (OPA/Rego) para tier de acesso (ex.: `infra-prod-admins` pode executar em prod, `time-x-readonly` só lê). A flag global de execução é o teto máximo; o grupo do chamador pode restringir ainda mais.
2. **Técnico (agente → API do cluster):** o ServiceAccount do agente tem só os verbs mínimos dos playbooks habilitados naquele cluster — nunca a união de todos os plugins existentes.
3. **Por risco da ação:** cada ação de cada playbook é tagueada `safe`/`medium`/`high`. Ações `high` (drenar nó, deletar PVC, mexer em CRD compartilhado) sempre forçam dry-run e aprovação humana, independente da flag global.

## Execução: escada de escalonamento (retry = 3 ações distintas)

Cada playbook define até 3 ações em ordem crescente de intervenção (ex.: restart pod → scale down/up → outra ação). Cada ação tem:

- **snapshot** do estado relevante antes de agir (só campos não sensíveis: replicas, tag de imagem, limits — nunca manifest completo)
- **execução** do comando
- **validação** (healthcheck confirma se resolveu)
- **rollback** próprio, caso essa ação precise ser desfeita

Se as 3 ações falharem, o engine faz rollback best-effort ao estado inicial e reporta ao usuário as 3 ações tentadas, outputs e motivo de cada falha — essa mesma trilha alimenta o post-mortem.

## Secrets e tokens: redação na origem

Nunca são impressos em log, output, audit trail ou no texto do post-mortem, em hipótese alguma:

- Redação acontece no `cluster-agent`, antes de qualquer dado sair do cluster.
- Playbooks são proibidos de ler `data`/`stringData` de Secret — só metadata.
- Um scrubber por regex (JWT, AWS keys, bearer tokens, etc.) roda sobre todo stdout/stderr de comando como segunda camada, independente da disciplina do autor do playbook.
- O snapshot de estado e o payload enviado ao LLM para gerar o post-mortem recebem só a versão já redigida.

## Distribuição: playbooks como artefato, não embutidos no binário

O `cluster-agent` é intencionalmente "burro": só transporte + execution engine genérico. Playbooks/plugins (core k8s, Calico, KEDA, ...) vivem em um repositório git, passam por PR review, e são publicados como artefato OCI versionado no ECR — mesmo padrão de um chart Helm. O ArgoCD (já existente na empresa) entrega tanto o binário do agente quanto o bundle de playbook a cada cluster via `ApplicationSet` com cluster generator, em ondas (canário → resto).

Como o Argo empurra o bundle para dentro do cluster, o diagnóstico read-only continua funcionando mesmo se a conexão com o hub cair — só a decisão de execução (RBAC + flag) depende do hub estar acessível.

## Filas

- **RabbitMQ** (já existente): ingestão de telemetria/eventos dos 1000 clusters para o motor de detecção preditiva. Topic exchange com routing key `cluster_id.plugin.severity` permite tanto ingestão quanto dispatch seletivo para workers por plugin.
- **SQS** (já existente): trabalho assíncrono gerenciado que integra com o resto do stack AWS (ex.: pipeline de post-mortem gravando em S3, acionando Lambda).

O canal de comando hub↔agente usa o stream gRPC persistente, não fila — a fila cobre o data-plane de telemetria, não o control-plane.

## Registry de artefatos: ECR

Infra nova, majoritariamente AWS. ECR evita operar mais um serviço stateful crítico do zero (vs. Harbor self-hosted), usa IAM já existente, reaproveita o mesmo caminho de rede que os nós já usam para puxar imagem, suporta artefato OCI (não só imagem) e replica entre regiões nativamente.
