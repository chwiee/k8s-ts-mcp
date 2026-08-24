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

**Uma ação de risco `high` nunca é executada automaticamente — isso é regra do próprio `execengine`, não só da política do hub.** Mesmo que o chamador esteja autorizado a executar (flag ligada, tier compatível), ao chegar numa ação `high` o engine grava a tentativa como `skipped` (com a `Description` do que precisaria ser rodado manualmente) e para a escada ali — não tenta as ações seguintes. Isso evita que uma escada com uma etapa segura no início e uma arriscada depois (ex.: `OOMKilled`: reiniciar o pod é seguro, aumentar o limite de memória não é) acabe travando a escada inteira em dry-run só por causa da etapa de risco — a etapa segura roda de verdade, a arriscada fica só como sugestão.

## Aprovação explícita de uma ação de alto risco

`execengine.Run` nunca executa uma ação `risk=high` — é a única forma de uma dessas rodar de verdade é a tool MCP `approve_action`, chamada explicitamente por um humano depois de ver a proposta de `troubleshoot`. O fluxo:

1. `troubleshoot` roda normalmente; toda a escada proposta (`ProposedAction`, incluindo nome/descrição/risco de cada rung) fica gravada no `audit.Entry` do incidente, junto com o `Signal` exato que foi diagnosticado — não só o texto já formatado que o usuário vê.
2. O usuário (ou o LLM em nome dele, mas sempre com confirmação explícita) chama `approve_action(incident_id, action_name)`.
3. O hub relê o incidente, confere que `action_name` realmente foi proposto **e** que o risco dele é `high` (uma ação `safe`/`medium` nunca precisou desse caminho — já teria rodado em `troubleshoot` se autorizada), confere que o chamador tem tier `prod-admin`, **e confere a flag global `--execution-enabled`** — um hub subido em modo só-proposta (o padrão nesta rodada de teste) recusa `approve_action` mesmo pra quem é `prod-admin`; a aprovação explícita adiciona uma segunda trava (o nome exato da ação + confirmação humana), ela não é uma forma de contornar a primeira (a flag do operador).
4. O hub manda `ApproveActionRequest{playbook_id, signal, action_name}` pro `cluster-agent` do cluster (mesmo padrão de forwarding entre réplicas do `Diagnose`/`Execute`/`Scan`, via `InternalService` quando necessário).
5. O `cluster-agent` **re-diagnostica o sinal do zero** (não confia em nenhum estado cacheado do primeiro `troubleshoot`) e confere que `playbook_id`/`action_name` ainda batem com a escada atual — se o estado do cluster mudou o suficiente pra apontar outro playbook, ou a ação não existe mais na escada, a aprovação falha em vez de rodar algo que não é mais o que foi proposto.
6. Só então `execengine.RunApproved` roda essa **uma** ação (nunca a escada inteira) — função deliberadamente separada de `Run`, que continua proibido de tocar `risk=high` mesmo sendo chamado internamente.
7. O resultado gera um **novo** `audit.Entry` (ligado ao incidente original via `original_incident_id` na resposta da tool), então tem post-mortem próprio.

**A revalidação do passo 5 não pode simplesmente re-rodar `Diagnose` no pod original quando um passo anterior da escada já mexeu na identidade desse pod.** `core/oomkilled` começa com "restart pod" (`kubectl delete pod`) — pelo tempo que um humano aprova o passo seguinte, o pod nomeado no `Signal` original quase sempre já foi substituído por um novo pod (nome gerado de novo pelo ReplicaSet), então re-`Diagnose`-ar esse pod exato falha com "not found". Achado ao vivo testando contra um pod `oomer` real em loop de OOM no `kind` — a primeira versão de `approve_action` quebrava sempre nesse caso, que é justamente o caso comum (qualquer playbook cujo primeiro passo reinicia o pod).

A correção: `internal/playbooks.Recheckable` é uma interface opcional — `core/oomkilled` a implementa com `Recheck(ctx, cli, sig, meta)`, que confere a saúde atual do **Deployment** (réplicas prontas vs. desejadas, usando `meta["deployment"]` capturado no `Diagnose` original) em vez de olhar o pod específico. `meta` viaja hub↔agente em `DiagnoseResponse`/`ApproveActionRequest` e fica persistido em `audit.Entry.Meta`, redigido do mesmo jeito que `Description`/`Summary`. Um playbook que não implementa `Recheckable` (ex: `core/execformaterror`, cuja escada é só uma ação diagnóstica de alto risco, sem restart antes) continua revalidando pelo pod normalmente — correto pra esse caso, já que nada mudou a identidade dele antes. Validado ao vivo: `approve_action` aumentou o limite de memória do deployment `oomer` de 50Mi para 75Mi contra um pod que já não existia mais no momento da aprovação.

## Runbooks: catálogo de referência para sinais sem playbook

Quando `Diagnose` não encontra nenhum playbook compilado pro sinal (`no_playbook=true`), o hub cai pra `internal/runbooks` — um Markdown versionado (`docs/runbooks/kubernetes-errors.md`, causa/diagnóstico/solução por tipo de erro) carregado com `--runbooks-path` e recarregado automaticamente a cada `--runbooks-reload-interval` (default 30s, pega edição sem reiniciar o hub). A busca tenta primeiro `kind` exato e cai para similaridade léxica (TF-IDF + cosseno, com stopwords em português filtradas) sobre título/keywords/corpo quando não há match exato. **Runbook nunca é executado — é sempre texto, sempre `dry_run=true`, independente de quem pergunta ou se a flag de execução está ligada**; é conhecimento de referência pro usuário aplicar manualmente, não mais um caminho de automação.

**Esse catálogo é o mecanismo pelo qual qualquer SRE adiciona uma nova tratativa de erro sem escrever Go nem esperar deploy** — só editar o Markdown (git, PR normal do time) e esperar o próximo reload automático. Duas entradas opcionais reforçam isso além de `kind`/`keywords`:

- **`log_signatures`**: uma ou mais substrings (case-insensitive) que, se aparecerem no log real, confirmam a entrada — em vez de confiar só na similaridade de texto entre a pergunta do usuário e o catálogo. Ex: a entrada de exec format error declara `exec format error, exec user process caused`.
- **`log_source`**: de onde vem esse log — `self` (padrão, o pod do próprio sinal) ou `namespace/deployment` fixo, pra quando o diagnóstico depende do log de um componente compartilhado, não do pod que o usuário reportou (ex: o KEDA operator, não o `ScaledObject` travado). Resolvido em tempo de consulta via `internal/k8sclient.PodForDeployment`, já que o nome do pod de um Deployment é gerado.

Quando `troubleshoot` cai no catálogo e a entrada encontrada por kind/keyword declara `log_signatures`, o hub busca esse log (via uma nova RPC `GetLogs`, mesmo padrão de forwarding entre réplicas de `Diagnose`/`Execute`/`Scan`/`ApproveAction`) e confirma antes de responder — a resposta final diz "confirmado pelo log" com o trecho encontrado, em vez de só uma correspondência por texto. Sem confirmação (fonte inacessível, ou log sem a assinatura), cai de volta pro texto simples da entrada — o catálogo continua útil mesmo sem log disponível, só sem a confirmação extra.

`docs/runbooks/kubernetes-errors.md` tem dois exemplos reais desse mecanismo: exec format error (`log_source: self`) e uma entrada nova, `KEDAOperatorError` (`log_source: keda-system/keda-operator`), que cobre um caso que hoje não tem playbook — falha no próprio `keda-operator`, não no `ScaledObject` — distinta da entrada `KEDAScaledObjectStuck` que já tem playbook compilado.

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

## Registro compartilhado entre réplicas do hub

Uma conexão de agente (o stream gRPC) só existe no processo que a aceitou — não dá pra "compartilhar" isso entre réplicas do `hub-server`. Rodar mais de uma réplica (necessário pros 1000+ clusters e ~500 usuários) exige então duas coisas separadas:

**1. Saber qual réplica tem qual cluster.** Cada réplica anuncia (`Announce`) e renova (`Heartbeat`, a cada ~10s, TTL ~30s) no Redis compartilhado (`internal/registry`, `ElastiCache` em produção) quais `cluster_id` ela tem conectados localmente agora. Uma réplica que cai sem se desconectar direito (crash) se autocura em até um TTL — ninguém precisa limpar entrada órfã manualmente. Quando uma chamada (`troubleshoot`) chega numa réplica que **não** tem o cluster localmente, ela consulta o Redis, descobre qual réplica tem, e **encaminha** a chamada pra lá via `InternalService` (gRPC réplica-a-réplica, unário, mesma porta `:7443`, mesmo mTLS — nunca exposto fora do namespace, restrito por `NetworkPolicy`). Quem chamou a tool nunca sabe que isso aconteceu. `list_clusters` também passou a consultar o Redis (visão de toda a frota), não só a réplica que atendeu a chamada.

**2. A própria sessão MCP também é por processo.** O protocolo Streamable HTTP guarda o handshake (`Mcp-Session-Id`) em memória, na réplica que recebeu o `initialize` — se as chamadas seguintes de uma mesma sessão caírem numa réplica diferente, a sessão quebra (`session not found`), *mesmo perguntando sobre um cluster conectado*. Isso é resolvido na camada de rede, não no código: o Service/Ingress na frente do `hub-server` precisa de **sticky session** (afinidade por sessão). Em produção (ALB/Ingress na AWS), isso é feito com stickiness por cookie, não por IP de origem — muitos dos ~500 usuários compartilham IP de saída (NAT corporativo), e afinidade por IP concentraria todo mundo numa réplica só.

Testado ao vivo (3 clusters reais em `kind`, 2 réplicas do hub, Redis real): uma réplica com **zero** conexões locais respondeu corretamente tanto `list_clusters` (via Redis) quanto `troubleshoot` completo — incluindo executar a remediação de verdade no cluster certo — via forwarding pra réplica dona da conexão.

## Segunda forma de alcançar um cluster: IAM Role da AWS, sem cluster-agent

Todo o resto deste documento descreve um cluster alcançado por um `cluster-agent` discando pra fora. Existe uma segunda forma, coexistindo com a primeira: o hub assume uma **IAM Role da AWS** e fala direto com a API do EKS, sem nenhum processo rodando dentro do cluster.

O ponto central do desenho: **nenhuma lógica de diagnóstico é duplicada**. `internal/agentcore.Handler` (Diagnose/Execute/Scan/ApproveAction/GetLogs) só depende de ter um `*k8sclient.Client` funcionando — nunca soube nem precisou saber que hoje ele sempre rodava dentro de um `cluster-agent` remoto. `internal/rolecluster.Manager` constrói esse mesmo `Handler`, só que com um `k8sclient.Client` vindo de `k8sclient.NewFromAWSRole` (assume a Role via STS, resolve endpoint/CA via `eks:DescribeCluster`, autentica cada chamada com um token no formato exato do `aws-iam-authenticator`, renovado sozinho a cada chamada por ele já expirar em ~15min) — e chama esse `Handler` **direto, como função Go, sem gRPC nenhum no meio**.

`internal/transport.Server` ganhou um campo `RoleResolver` (interface pequena, só pra não criar import cycle com `internal/rolecluster`) checado antes do registro de agentes gRPC em toda chamada (`Diagnose`/`Execute`/`Scan`/`ApproveAction`/`GetLogs`) — um `cluster_id` que o `RoleResolver` não conhece cai pro caminho de sempre, sem mudança de comportamento. Configurado via `--role-clusters-config` (YAML com `cluster_id`/`role_arn`/`eks_cluster_name`/`region` por cluster) — vazio desliga esse caminho inteiramente. Clusters via Role não precisam do Redis compartilhado entre réplicas: qualquer réplica monta o `Handler` sozinha, sem depender de "qual réplica tem a conexão".

Validado ao vivo contra [Floci](https://floci.io) (emulador de AWS local, compatível com LocalStack) — que de fato sobe um container `rancher/k3s` real por cluster EKS emulado e valida o token do jeito que o EKS de verdade faz — com o `hub-server` rodando os dois caminhos ao mesmo tempo: um cluster `kind` via `cluster-agent`/gRPC e um cluster (`probe-cluster`) via Role/Floci, ambos respondendo `scan_cluster`/`troubleshoot` corretamente, incluindo um erro real (`pods "x" not found`) vindo da API de verdade quando perguntado sobre um pod inexistente — prova de que não é um stub.

## Estado atual da implementação

Implementado e testado (`go test ./...` verde): `internal/policy` (OPA embutido), `internal/execengine` (escada + rollback + `RunApproved` pra aprovação explícita de ação `risk=high`), `internal/redact`, `internal/audit` (FileStore, agora com `Signal`/`Meta`/`ProposedActions` estruturados por incidente), `internal/postmortem`, `internal/registry` (InMemory + Redis, mesmo contrato testado nos dois), `internal/runbooks` (Markdown + busca por similaridade TF-IDF + reload automático + confirmação por log via `log_signatures`/`log_source`), `internal/transport` (gRPC mTLS, registro compartilhado + forwarding entre réplicas — incluindo `ApproveAction` e `GetLogs` — testado com bufconn simulando múltiplos processos), `internal/k8sclient` + `internal/playbooks/{corek8s,calico,keda}` (client-go real; `corek8s` cobre CrashLoopBackOff, OOMKilled, ImagePullBackOff e ExecFormatError — este último agora também detectado heuristicamente pelo `scan_cluster` a partir do log quando a API do Kubernetes não expõe mensagem —; `core/oomkilled` implementa `playbooks.Recheckable`), `internal/mcptools` (tools MCP `list_clusters`/`scan_cluster`/`troubleshoot`/`approve_action`/`get_postmortem`, testado ponta-a-ponta e também ao vivo contra um cluster `kind` real), `internal/rolecluster` + `internal/k8sclient.NewFromAWSRole` (segunda forma de alcançar um cluster, via IAM Role da AWS — roteamento testado com fakes, credencial/token testado ao vivo contra Floci), `cmd/hub-server` e `cmd/cluster-agent` (ambos com modo `--insecure` para dev local). No `wb-ce-agent` (repositório irmão): `internal/predictive` faz polling periódico de `list_clusters`/`scan_cluster` e avisa proativamente por WhatsApp sobre problemas novos, nunca diagnostica/remedia sozinho.

Gaps conhecidos, ainda não resolvidos:

- **Identidade do chamador**: os grupos AD do requisitante ainda não são propagados de verdade — `--test-caller-groups` no hub-server fixa um grupo só, pendente de como o `wb-ce-agent` (ou outro chamador) vai carregar a identidade real na chamada MCP.
- **Emissão/rotação de certificado mTLS por cluster**: não construída — hoje são flags apontando pra arquivos de cert/key/CA que alguém já provisionou.
- **Endpoint MCP HTTP sem TLS/autenticação própria** ainda — hoje é HTTP puro, pensado pra rodar atrás de rede interna, e depende do Ingress real ter sticky session por cookie (não testado ainda — o teste local em `kind` usou `sessionAffinity: ClientIP` do Service como stand-in, documentado como não-ideal pra produção acima).
- **`ClusterEnv` (prod/nonprod) por cluster** vem de uma flag CSV (`--prod-clusters`) no hub-server, não de um inventário — suficiente pros testes locais, não pra produção real.
