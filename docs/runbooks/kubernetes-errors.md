# Catálogo de erros do Kubernetes

Referência de problemas comuns em Kubernetes: causa, como diagnosticar, e o que normalmente resolve. Isso é **conhecimento, não automação** — nada aqui roda sozinho. O objetivo é que o `k8s-ts-mcp` (ou qualquer pessoa) proponha a solução como texto quando não existir um playbook automatizado (`internal/playbooks`) pra aquele problema — quem decide e executa é sempre um humano.

Playbooks automatizados hoje cobrem `core/crashloopbackoff`, `core/oomkilled`, `core/imagepullbackoff`, `core/execformaterror` (diagnóstico, nunca executa nada), `calico/node-degraded` e `keda/scaledobject-stuck`. Tudo mais neste catálogo é proposta, não ação — e mesmo pra esses seis, a entrada correspondente aqui no catálogo continua existindo como referência lida por humano, já que o `troubleshoot` prioriza o playbook compilado quando ele existe.

## Formato

Cada entrada segue a mesma estrutura, pensada pra ficar fácil de indexar/buscar no futuro sem precisar reescrever nada:

```
[2 sustenidos] Título
kind: NomeDoSinal (se houver um equivalente formal; vazio se for só referência)
keywords: palavras soltas usadas pra achar essa entrada numa busca
log_signatures: trechos de texto (opcional) que, se aparecerem no log, confirmam essa entrada
log_source: self (padrão, log do próprio pod do sinal) OU namespace/deployment (log de um componente fixo, ex: keda-system/keda-operator)

**Causa comum**: ...
**Diagnóstico**: comandos pra confirmar
**Solução**: passos pra resolver
```

(Escrito como "[2 sustenidos]" aqui de propósito, pra esse exemplo não ser confundido com uma entrada de verdade por quem for indexar o arquivo — nas entradas reais abaixo é `##` mesmo.)

**`log_signatures`/`log_source` são opcionais** e existem pra deixar o catálogo mais esperto sem precisar de código Go novo: quando `troubleshoot` cai no catálogo (nenhum playbook compilado pro sinal) e a entrada encontrada por `kind`/palavra-chave declara `log_signatures`, o hub busca o log da fonte indicada e confirma (ou não) contra essas assinaturas antes de responder — a resposta final diz claramente se foi "confirmado pelo log" ou só uma correspondência por texto. Qualquer SRE pode adicionar uma assinatura nova editando só o markdown; o hub recarrega sozinho em até 30s (`--runbooks-reload-interval`), sem precisar de deploy.

**Três exemplos comentados, pra quem for escrever uma entrada nova e quiser ver o padrão em ação:**
- **Só `kind`/`keywords`, sem log** (o caso mais comum — a maioria dos problemas se diagnostica pela descrição/eventos do recurso, sem precisar ler log nenhum): `PodPending` mais abaixo.
- **`log_signatures` com `log_source: self`** (confirma contra o log do próprio pod do sinal): `PodExecFormatError` mais abaixo.
- **`log_signatures` com `log_source` fixo** (o problema não está no pod que o usuário reportou, e sim num componente compartilhado — controller, operator, webhook): `KEDAOperatorError` mais abaixo, e as novas `CoreDNSLoopDetected`/`CertificateExpired`/`ImagePullRateLimited` no fim do arquivo.

---

## Pod em CrashLoopBackOff (causas gerais)
kind: PodCrashLoopBackOff
keywords: crashloopbackoff, crash loop, reiniciando, restart, back-off restarting

**Causa comum**: o container inicia e morre repetidamente. O motivo varia — comando/entrypoint errado, dependência (banco, outro serviço) fora do ar na inicialização, erro de configuração, falta de variável de ambiente obrigatória, ou a aplicação genuinamente crashando por bug.

**Diagnóstico**:
- `kubectl logs <pod> -n <ns> --previous` — o log da tentativa anterior (o pod atual pode estar reiniciando rápido demais pra pegar log útil)
- `kubectl describe pod <pod> -n <ns>` — olhar `Last State`, `Reason`, `Exit Code`, e os `Events` no fim
- Exit code 1 = erro genérico da aplicação; 137 = SIGKILL (frequentemente OOM, ver entrada própria); 143 = SIGTERM; 126/127 = comando não executável/não encontrado

**Solução**: depende do exit code e do log. Comando/entrypoint errado → corrigir o `command`/`args` no manifesto. Dependência não pronta → adicionar `initContainers` que espera a dependência, ou ajustar `readinessProbe`/`startupProbe`. Bug da aplicação → precisa de fix no código, não é operável só por infra. O playbook automatizado (`core/crashloopbackoff`) cobre o caso mais simples (reiniciar resolve) — quando não resolve, o problema é um destes acima.

---

## Pod morto por falta de memória (OOMKilled)
kind: PodOOMKilled
keywords: oomkilled, oom, out of memory, memory limit, sem memoria

**Causa comum**: o container excedeu o `resources.limits.memory` configurado (ou o limite do namespace/node) e o kernel matou o processo.

**Diagnóstico**:
- `kubectl describe pod <pod> -n <ns>` — `Last State: Terminated, Reason: OOMKilled`
- Comparar o uso real (`kubectl top pod <pod> -n <ns>`, se o metrics-server estiver instalado) contra o limite configurado
- Repetição frequente do OOM no mesmo container, mesmo após reiniciar, é sinal de vazamento de memória, não de um limite só um pouco baixo

**Solução**: se o limite está genuinamente baixo pra carga real da aplicação, aumentar `resources.limits.memory` (e provavelmente `requests.memory` junto). Se está subindo sem parar mesmo com limite razoável, investigar vazamento de memória na aplicação — aumentar o limite só adia o problema. O playbook automatizado (`core/oomkilled`) tenta reiniciar (cobre picos transitórios) e propõe aumento de limite como sugestão de texto — nunca aplica sozinho, porque o valor certo exige julgamento humano.

---

## Pod não consegue baixar a imagem (ImagePullBackOff / ErrImagePull)
kind: PodImagePullBackOff
keywords: imagepullbackoff, errimagepull, pull image, imagem não encontrada, unauthorized, manifest unknown

**Causa comum**: imagem/tag não existe no registry, nome digitado errado, o cluster não tem credencial pra puxar de um registry privado (`imagePullSecrets` ausente/errado), ou o registry está fora do ar/rate-limitando.

**Diagnóstico**:
- `kubectl describe pod <pod> -n <ns>` — a mensagem de erro no evento `Failed` geralmente já diz o motivo exato: `manifest unknown` (tag não existe), `unauthorized`/`pull access denied` (sem credencial ou credencial errada), `no such host`/timeout (registry inacessível)
- Confirmar o nome/tag da imagem contra o que está publicado de fato no registry

**Solução**: `manifest unknown` → corrigir o nome/tag no manifesto, ou publicar a imagem que falta. `unauthorized` → conferir se `imagePullSecrets` está configurado no pod/ServiceAccount e se a credencial não expirou. Registry inacessível → checar conectividade de rede do nó até o registry (proxy, firewall, NetworkPolicy). O playbook automatizado (`core/imagepullbackoff`) só tenta reiniciar (cobre falha transitória, tipo rate limit passageiro) — motivo real de imagem inexistente/sem credencial precisa de correção manual do manifesto ou da credencial.

---

## Container não executa o entrypoint (exec format error / binário incompatível)
kind: PodExecFormatError
keywords: exec format error, exec user process caused, arquitetura, architecture, arm64, amd64, wrong platform
log_signatures: exec format error, exec user process caused
log_source: self

**Causa comum**: a imagem foi buildada pra uma arquitetura de CPU diferente da do nó (ex: imagem `arm64` agendada num nó `amd64`), ou o binário do entrypoint está corrompido/não é um executável válido.

**Diagnóstico**:
- `kubectl describe pod <pod> -n <ns>` — exit code costuma ser 255 ou 8; a mensagem "exec format error" nem sempre aparece nos campos que a API do Kubernetes expõe, dependendo do runtime do nó
- `kubectl get node <node> -o jsonpath='{.status.nodeInfo.architecture}'` — arquitetura do nó onde o pod rodou
- Verificar a arquitetura real da imagem: `docker manifest inspect <imagem>` ou `docker inspect <imagem> --format '{{.Architecture}}'`

**Solução**: se o cluster tem nó com a arquitetura que a imagem realmente é, um `nodeAffinity`/`nodeSelector` (`kubernetes.io/arch`) resolve sem precisar rebuildar nada. Se não tem, rebuildar a imagem pra arquitetura certa: `docker buildx build --platform linux/amd64,linux/arm64 --push` (multi-arch, resolve pra qualquer nó). O playbook automatizado (`core/execformaterror`) já checa a arquitetura real do nó e do cluster via API e propõe qual dos dois caminhos se aplica — mas é **só diagnóstico, nunca executa nada sozinho**, porque tanto rebuildar quanto reagendar via afinidade são decisões que dependem de contexto que só um humano tem (qual é a arquitetura real da imagem, se faz sentido ter multi-arch, etc.).

---

## Pod preso em Pending (não consegue ser agendado)
kind: PodPending
keywords: pending, unschedulable, insufficient cpu, insufficient memory, taint, toleration, node affinity, no nodes available

**Causa comum**: nenhum nó satisfaz os requisitos do pod — falta de CPU/memória disponível, `nodeSelector`/`nodeAffinity` que nenhum nó atende, taint sem toleration correspondente, ou PVC que o pod depende ainda não está `Bound`.

**Diagnóstico**:
- `kubectl describe pod <pod> -n <ns>` — a seção `Events` mostra exatamente por que o scheduler rejeitou cada nó (ex: `Insufficient cpu`, `node(s) had taint {...}, that the pod didn't tolerate`, `didn't match Pod's node affinity/selector`)
- `kubectl get nodes -o wide` e `kubectl describe node <node>` pra ver capacidade/uso e taints de cada nó
- `kubectl get pvc -n <ns>` se o pod usa volume — conferir se está `Bound`

**Solução**: falta de recurso → aumentar a capacidade do cluster (novo nó) ou reduzir `requests` do pod, se estiver superdimensionado. Taint sem toleration → adicionar a `toleration` correspondente ao pod, se ele realmente deve rodar ali. Afinidade impossível de satisfazer → corrigir o `nodeSelector`/`nodeAffinity` pra refletir os nós que existem de fato. PVC não `Bound` → ver a entrada de armazenamento abaixo.

---

## Pod preso em ContainerCreating
kind: PodContainerCreating
keywords: containercreating, container creating, mount failed, secret not found, configmap not found

**Causa comum**: demora/falha ao montar um volume, `Secret`/`ConfigMap` referenciado no pod não existe (ou está no namespace errado), problema no plugin de rede (CNI) do nó, ou a imagem está demorando muito pra baixar (nesse caso ver a entrada de ImagePullBackOff).

**Diagnóstico**:
- `kubectl describe pod <pod> -n <ns>` — eventos tipo `FailedMount`, `secret "x" not found`, `configmap "x" not found` apontam a causa direto
- Confirmar que o `Secret`/`ConfigMap` referenciado existe no mesmo namespace do pod (`kubectl get secret/configmap -n <ns>`)

**Solução**: criar o `Secret`/`ConfigMap` que falta (ou corrigir o nome referenciado no pod), no namespace certo. Problema de CNI/rede do nó geralmente exige olhar os logs do daemonset da rede (ex: `calico-node`, ver entrada de Calico) e, às vezes, reiniciar o pod da CNI naquele nó especificamente.

---

## Pod Evicted
kind: PodEvicted
keywords: evicted, disk pressure, memory pressure, node pressure

**Causa comum**: o kubelet do nó expulsou o pod porque o nó ficou sob pressão de recurso — geralmente disco cheio (`DiskPressure`) ou memória do nó (não do container) esgotada (`MemoryPressure`).

**Diagnóstico**:
- `kubectl get pod <pod> -n <ns> -o jsonpath='{.status.reason}'` — deve mostrar `Evicted`, e `.status.message` traz o motivo
- `kubectl describe node <node>` — a seção `Conditions` mostra se o nó está (ou esteve) com `DiskPressure`/`MemoryPressure` = `True`
- Um pod `Evicted` não volta sozinho — o controlador (Deployment/ReplicaSet) cria um novo, o antigo fica só como registro histórico

**Solução**: liberar espaço em disco/memória no nó (limpar imagens antigas com `docker/crictl system prune`, investigar o que está consumindo), ou adicionar capacidade. Se um único pod está consumindo desproporcionalmente, ajustar `resources.limits` dele pra não afetar o nó inteiro. Apagar o pod antigo `Evicted` é seguro (`kubectl delete pod <pod> -n <ns>`) — ele já não representa nada rodando.

---

## Erro ao criar o container (CreateContainerConfigError / CreateContainerError)
kind: PodCreateContainerError
keywords: createcontainerconfigerror, createcontainererror, invalid securitycontext, env from secret

**Causa comum**: `CreateContainerConfigError` quase sempre é referência a `Secret`/`ConfigMap` que não existe, usado via `envFrom`/`valueFrom` (diferente do erro de montagem de volume, esse é especificamente de variável de ambiente). `CreateContainerError` costuma ser `securityContext` inválido (ex: pedir uma capability que o nó não permite) ou runtime do container rejeitando a config.

**Diagnóstico**:
- `kubectl describe pod <pod> -n <ns>` — evento traz o nome exato do `Secret`/`ConfigMap`/chave que falta, ou a mensagem específica do runtime

**Solução**: criar o `Secret`/`ConfigMap` (ou a chave dentro dele) que falta, no namespace certo. Pra `securityContext`, ajustar as capabilities/config pedidas pra algo que o nó realmente permite (ou pedir liberação ao time de plataforma se for uma restrição de PodSecurityPolicy/admission controller).

---

## Init container falhando (Init:Error / Init:CrashLoopBackOff)
kind: PodInitContainerError
keywords: init:error, init container, init:crashloopbackoff

**Causa comum**: o pod tem um ou mais `initContainers`, e um deles está falhando — os containers principais nunca chegam a iniciar até todo init container terminar com sucesso.

**Diagnóstico**:
- `kubectl logs <pod> -n <ns> -c <nome-do-init-container>` — logs são por container, precisa especificar qual init container
- `kubectl describe pod <pod> -n <ns>` — mostra qual init container está falhando e em que posição da sequência

**Solução**: tratar como um `CrashLoopBackOff` normal, mas focado no init container específico — ver a entrada de CrashLoopBackOff pra causas comuns. É frequente ser espera de dependência (banco, outro serviço) que nunca fica pronta — nesse caso, checar se a dependência realmente está saudável e acessível a partir do cluster.

---

## Pod Running mas não Ready (falha de probe)
kind: PodNotReady
keywords: not ready, readiness probe failed, liveness probe failed, 0/1 running

**Causa comum**: o container está rodando (não crashou), mas o `readinessProbe` está falhando — então o pod nunca entra no `Service` como endpoint válido. Pode ser a aplicação genuinamente não pronta (ainda inicializando, dependência lenta), ou a probe mal configurada (porta/path errados, timeout curto demais).

**Diagnóstico**:
- `kubectl describe pod <pod> -n <ns>` — eventos `Readiness probe failed` mostram a resposta (ou timeout) exata
- `kubectl get pod <pod> -n <ns>` — coluna `READY` mostra `0/1` mesmo com `STATUS Running`
- Testar a probe manualmente: `kubectl exec <pod> -n <ns> -- curl -sf localhost:<porta><path>` (ajustar pro tipo de probe configurado)

**Solução**: se a aplicação demora legitimamente pra ficar pronta, aumentar `initialDelaySeconds`/`failureThreshold`, ou usar `startupProbe` separado. Se a probe está testando o endpoint errado, corrigir `path`/`port` no manifesto. Se a aplicação nunca fica pronta de verdade, o problema é na aplicação (dependência inacessível, erro de config), não na probe em si.

---

## Pod preso em Terminating (não sai)
kind: PodStuckTerminating
keywords: terminating, stuck terminating, finalizer, force delete

**Causa comum**: o pod tem um `finalizer` esperando alguma ação externa (ex: um controller de storage precisa desmontar o volume primeiro) que não está completando, ou o kubelet do nó está inacessível pra confirmar a finalização.

**Diagnóstico**:
- `kubectl get pod <pod> -n <ns> -o yaml` — olhar `metadata.finalizers` (o que está travando) e `metadata.deletionTimestamp` (há quanto tempo está tentando)
- Verificar se o nó onde o pod está está `Ready` (`kubectl get nodes`) — nó inacessível trava a finalização

**Solução**: identificar e resolver o motivo do finalizer (ex: o controller de storage/CNI correspondente pode estar travado e precisar de restart). Como último recurso, remover o finalizer manualmente (`kubectl patch pod <pod> -n <ns> -p '{"metadata":{"finalizers":[]}}' --type=merge`) força a remoção — mas só depois de confirmar que não vai deixar recurso órfão (volume não desmontado, IP não liberado), porque isso pula a limpeza que o finalizer deveria garantir.

---

## Nó NotReady
kind: NodeNotReady
keywords: node notready, node not ready, kubelet down

**Causa comum**: o kubelet do nó parou de reportar (crash, problema de rede entre o nó e o control plane), o próprio nó está com problema de sistema operacional/recurso, ou houve uma partição de rede.

**Diagnóstico**:
- `kubectl describe node <node>` — seção `Conditions`, campo `Ready` e a mensagem associada; `LastHeartbeatTime` muito atrasado indica o kubelet parou de reportar
- Se possível, acessar o nó diretamente (SSH/console) e checar `systemctl status kubelet`, uso de disco/memória, conectividade de rede até o control plane

**Solução**: reiniciar o serviço do kubelet no nó, se ele crashou. Se o nó está com recurso esgotado, liberar espaço/memória ou substituir o nó. Problema de rede entre nó e control plane precisa de investigação de infraestrutura (firewall, rota, DNS). Pods que estavam no nó são reagendados automaticamente pelos controllers depois que o Kubernetes considera o nó definitivamente indisponível (alguns minutos por padrão) — não force delete de pods de um nó `NotReady` sem confirmar que o nó realmente não vai voltar, pra evitar dois pods do mesmo StatefulSet/volume rodando ao mesmo tempo.

---

## PVC preso em Pending
kind: PVCPending
keywords: pvc pending, no persistent volumes available, storageclass, waiting for a volume to be created

**Causa comum**: não existe `PersistentVolume` compatível disponível, a `StorageClass` referenciada não existe (ou o provisionador dela está com problema), ou o provisionador dinâmico de storage não está funcionando.

**Diagnóstico**:
- `kubectl describe pvc <nome> -n <ns>` — eventos mostram o motivo (`no persistent volumes available for this claim`, erro do provisionador, etc.)
- `kubectl get storageclass` — confirmar que a `StorageClass` pedida existe
- Verificar se o controller/CSI driver do provisionador está `Running` (`kubectl get pods -n <namespace-do-csi-driver>`)

**Solução**: criar/corrigir a `StorageClass` referenciada, ou provisionar manualmente um `PersistentVolume` compatível se o cluster não tem provisionamento dinâmico. Se o CSI driver está com problema, tratar como troubleshooting normal de pod (ver logs, reiniciar).

---

## Falha de resolução de DNS dentro do cluster
kind: ClusterDNSFailure
keywords: dns, coredns, could not resolve host, nxdomain, name resolution

**Causa comum**: `CoreDNS` (ou o DNS do cluster equivalente) está com problema — pods insuficientes, crashando, ou com configuração de upstream errada — ou uma `NetworkPolicy` está bloqueando tráfego até o DNS.

**Diagnóstico**:
- `kubectl get pods -n kube-system -l k8s-app=kube-dns` (ou o label equivalente) — confirmar que os pods do CoreDNS estão `Running`
- `kubectl logs -n kube-system -l k8s-app=kube-dns` — erros de configuração ou de upstream costumam aparecer aqui
- Testar de dentro de um pod: `kubectl exec <pod> -n <ns> -- nslookup kubernetes.default`

**Solução**: reiniciar os pods do CoreDNS se estiverem instáveis (o Deployment recria). Corrigir o `ConfigMap` do CoreDNS se o upstream DNS estiver errado. Se for `NetworkPolicy` bloqueando, garantir uma regra explícita permitindo tráfego (UDP/TCP 53) até o namespace/pods do DNS.

---

## Service sem endpoints / tráfego não chega
kind: ServiceNoEndpoints
keywords: no endpoints, service unreachable, connection refused, selector mismatch

**Causa comum**: o `selector` do `Service` não bate com os labels de nenhum pod (erro de digitação mais comum), ou nenhum pod que bate com o selector está `Ready` (Service só inclui pods prontos como endpoint).

**Diagnóstico**:
- `kubectl get endpoints <service> -n <ns>` — lista vazia confirma o problema
- `kubectl get pods -n <ns> --show-labels` comparado com `kubectl get svc <service> -n <ns> -o yaml` (campo `spec.selector`) — conferir se realmente batem
- Se os labels batem mas ainda assim está vazio, o problema é os pods não estarem `Ready` (ver entrada de probe acima)

**Solução**: corrigir o `selector` do Service (ou os labels dos pods) pra baterem de verdade. Se é problema de readiness, resolver a causa raiz da probe falhando.

---

## NetworkPolicy bloqueando tráfego inesperadamente
kind: NetworkPolicyBlocking
keywords: network policy, connection timeout, blocked traffic, denied by policy

**Causa comum**: uma `NetworkPolicy` existente (às vezes criada por outro time, ou uma policy "default deny" do namespace) está bloqueando tráfego que deveria ser permitido — comum depois que um namespace adota isolamento de rede e as policies de permissão específicas não cobrem todos os fluxos necessários.

**Diagnóstico**:
- `kubectl get networkpolicy -n <ns>` — listar todas as policies afetando o namespace
- Revisar cada uma contra o fluxo de tráfego esperado (origem, destino, porta) — lembrando que NetworkPolicies são aditivas: se existe qualquer policy de `Ingress`/`Egress` selecionando o pod, só o que está explicitamente permitido passa, o resto é negado por padrão

**Solução**: adicionar uma regra explícita permitindo o fluxo necessário (origem/porta certos), em vez de remover a policy inteira — isso mantém o resto do isolamento de segurança intacto.

---

## Nó do Calico degradado / problema de rede entre pods
kind: CalicoNodeDegraded
keywords: calico, bgp, calico-node, cross-node networking, pod to pod unreachable

**Causa comum**: o `calico-node` de um nó específico está com problema (BGP não estabelecido com os outros nós, ou o próprio pod do Calico crashando), afetando conectividade pod-a-pod cujo tráfego passa por aquele nó.

**Diagnóstico**:
- `kubectl get pods -n calico-system -o wide` (ou `kube-system`, depende de como foi instalado) — conferir se o `calico-node` daquele nó está `Running`
- `calicoctl node status` (se disponível) no nó afetado — mostra o estado das sessões BGP com os outros nós

**Solução**: o playbook automatizado (`calico/node-degraded`) já cobre o primeiro passo — reiniciar o `calico-node` daquele nó específico, que é o runbook padrão de troubleshooting do próprio Calico pra resetar a malha BGP. Se não resolver, o problema é mais profundo (rede física entre nós, configuração de BGP) e exige olhar a topologia de rede real.

---

## ScaledObject do KEDA travado
kind: KEDAScaledObjectStuck
keywords: keda, scaledobject, hpa, not scaling, stuck scaler

**Causa comum**: o `ScaledObject` do KEDA para de escalar corretamente — o `HorizontalPodAutoscaler` que o KEDA gerencia fica com estado inconsistente, geralmente depois de uma falha temporária de conexão com a fonte de métrica (ex: fila, banco).

**Diagnóstico**:
- `kubectl get scaledobject <nome> -n <ns> -o yaml` — campo `status.conditions`, procurar `Ready: False` e a mensagem associada
- `kubectl get hpa -n <ns>` — conferir se o HPA gerenciado pelo KEDA existe e está atualizado

**Solução**: o playbook automatizado (`keda/scaledobject-stuck`) já cobre a correção padrão — apagar o HPA que o KEDA criou (`kubectl delete hpa <nome> -n <ns>`), o que força o KEDA a recriá-lo do zero no próximo reconcile. Isso é seguro porque o HPA é só um recurso derivado que o KEDA gerencia — apagá-lo não afeta os pods em si.

---

## KEDA não escala e o ScaledObject não é o problema (falha no operator)

kind: KEDAOperatorError
keywords: keda, escalonamento, scaling, operator, controller, trigger auth, external metrics
log_signatures: failed to get scaler, error getting scale target, unable to sync deployment, failed to resolve auth params
log_source: keda-system/keda-operator

**Causa comum**: diferente do `KEDAScaledObjectStuck` acima (onde o `HorizontalPodAutoscaler` derivado fica com estado inconsistente), aqui o problema é no próprio `keda-operator` — geralmente falha de autenticação com a fonte externa de métrica (`TriggerAuthentication` expirado/errado) ou a fonte de métrica em si inacessível. Isso não tem playbook automatizado ainda: o sintoma pro usuário ("meu KEDA não está escalando") é igual ao caso acima, então quem diagnostica precisa olhar o log do `keda-operator`, não do `ScaledObject`/pod da aplicação — por isso essa entrada usa `log_source` apontando pro operator em vez do sinal original.

**Diagnóstico**: `kubectl logs -n keda-system deploy/keda-operator --tail=50` — procurar por falha ao autenticar no `TriggerAuthentication` ou ao consultar a fonte de métrica externa.

**Solução**: depende da causa exata no log — credencial expirada/errada no `TriggerAuthentication`, endpoint da métrica inacessível (rede/DNS), ou o próprio KEDA operator reiniciando em loop (nesse caso, ver o próprio pod do operator com `core/crashloopbackoff`/`core/oomkilled`, que aí sim têm playbook). Sem automação por enquanto — é só diagnóstico.

---

## Quota de recursos excedida (ResourceQuota)
kind: ResourceQuotaExceeded
keywords: exceeded quota, resourcequota, forbidden, quota exceeded

**Causa comum**: o namespace tem uma `ResourceQuota` configurada, e criar/escalar um recurso ultrapassaria o limite (CPU, memória, número de pods, etc.) definido pra aquele namespace.

**Diagnóstico**:
- A mensagem de erro ao tentar criar o recurso já diz isso diretamente: `exceeded quota: <nome>, requested: ..., used: ..., limited: ...`
- `kubectl describe resourcequota -n <ns>` — mostra uso atual vs. limite de cada recurso controlado

**Solução**: reduzir o `requests`/`limits` do recurso sendo criado, se estiver superdimensionado, ou pedir aumento da quota do namespace ao time responsável por definir esses limites (geralmente é uma decisão deliberada de capacidade, não pra ser simplesmente contornada).

---

## Acesso negado por RBAC (Forbidden)
kind: RBACForbidden
keywords: forbidden, rbac, permission denied, cannot get, cannot list, cannot create, unauthorized, serviceaccount

**Causa comum**: o `ServiceAccount` (de um pod, de um usuário via `kubectl`, ou de uma pipeline de CI/CD) não tem um `Role`/`ClusterRole` com o verbo (`get`, `list`, `create`, `delete`, ...) e recurso necessários vinculados via `RoleBinding`/`ClusterRoleBinding`. Muito comum logo depois de subir uma aplicação nova que passou a precisar falar com a API do Kubernetes (ex: um operator, um controller, algo usando `client-go`).

**Diagnóstico**:
- A mensagem de erro já diz exatamente o que faltou: `User "system:serviceaccount:<ns>:<sa>" cannot <verbo> resource "<recurso>" in API group "<grupo>"`
- `kubectl auth can-i <verbo> <recurso> --as=system:serviceaccount:<ns>:<sa> -n <ns>` — reproduz a checagem de permissão isoladamente, sem precisar re-rodar a aplicação inteira
- `kubectl get rolebinding,clusterrolebinding -A -o wide | grep <sa>` — mostra quais bindings (se algum) já existem pra esse ServiceAccount

**Solução**: criar/ajustar o `Role` (escopo de namespace) ou `ClusterRole` (escopo de cluster) com o verbo/recurso faltando, e o `RoleBinding`/`ClusterRoleBinding` associando ao `ServiceAccount`. Preferir sempre o escopo mais restrito que resolve (namespace em vez de cluster inteiro) — dar `ClusterRole` amplo de forma reativa pra "parar de dar erro" é o tipo de decisão que vira problema de segurança depois.

---

## Webhook de admissão bloqueando criação/alteração de recursos
kind: AdmissionWebhookBlocking
keywords: admission webhook denied, webhook, validatingwebhook, mutatingwebhook, timeout calling webhook, connection refused webhook

**Causa comum**: um `ValidatingWebhookConfiguration`/`MutatingWebhookConfiguration` (Istio, cert-manager, Gatekeeper/OPA, um webhook interno do time) intercepta a criação/alteração do recurso e rejeita — ou o próprio webhook está fora do ar/inacessível, o que trava a operação mesmo sem ter nada a ver com o recurso em si.

**Diagnóstico**:
- A mensagem de erro identifica o webhook: `Error from server: admission webhook "<nome>" denied the request: ...` (rejeição deliberada, motivo geralmente já vem na mensagem) ou `... failed calling webhook "<nome>": ... context deadline exceeded / connection refused` (o webhook em si está inacessível)
- `kubectl get validatingwebhookconfigurations,mutatingwebhookconfigurations` — lista os webhooks ativos no cluster
- Se o webhook está inacessível: verificar se o `Service`/pod que o implementa está saudável (`kubectl get pods -n <ns-do-webhook>`)

**Solução**: se foi rejeição deliberada, a mensagem do webhook já diz o que precisa mudar no recurso (label obrigatória faltando, política do Gatekeeper violada, etc.). Se o webhook está inacessível, o problema é troubleshooting normal de pod/serviço nesse namespace (provavelmente cai em `PodCrashLoopBackOff`/`ServiceNoEndpoints` acima) — uma vez saudável, as criações voltam a passar. Em emergência, um webhook com `failurePolicy: Ignore` não bloqueia quando inacessível — mas mudar isso é decisão de quem é dono do webhook, não algo pra fazer reativamente no meio de um incidente.

---

## HPA nativo não escala (sem métricas)
kind: HPAMissingMetrics
keywords: hpa, horizontalpodautoscaler, unknown, unable to get metrics, metrics-server, failedgetresourcemetric

**Causa comum**: diferente de `KEDAScaledObjectStuck`/`KEDAOperatorError` acima (que são especificamente do KEDA), este é o `HorizontalPodAutoscaler` nativo do Kubernetes — ele depende do `metrics-server` (ou de uma métrica customizada via Prometheus Adapter) pra saber o uso atual de CPU/memória. Se o `metrics-server` está fora do ar, com erro, ou os `resources.requests` do deployment-alvo não estão definidos (o HPA de CPU/memória não funciona sem `requests` configurado), o HPA fica sem saber pra que lado escalar.

**Diagnóstico**:
- `kubectl get hpa -n <ns>` — coluna `TARGETS` mostrando `<unknown>/<valor>` é o sintoma direto
- `kubectl describe hpa <nome> -n <ns>` — evento `FailedGetResourceMetric` ou `FailedComputeMetricsReplicas` com o motivo
- `kubectl get pods -n kube-system -l k8s-app=metrics-server` (ou o namespace onde o metrics-server roda nesse cluster) — confirmar que está `Running`
- `kubectl top pods -n <ns>` — se isso também falhar, confirma que o `metrics-server` é o problema, não o HPA em si

**Solução**: se o `metrics-server` está com problema, é troubleshooting normal de pod (`PodCrashLoopBackOff`/`PodOOMKilled` acima). Se está saudável mas o HPA continua `<unknown>`, confirmar que o deployment-alvo tem `resources.requests` de CPU/memória definido — sem isso o HPA não tem base de cálculo, mesmo com o metrics-server funcionando perfeitamente.

---

## Cluster Autoscaler não sobe nó novo (pod Pending por falta de capacidade)
kind: ClusterAutoscalerStuck
keywords: cluster autoscaler, pending, insufficient, node group, scale up, max size reached, no available node group

**Causa comum**: diferente do `PodPending` genérico acima (que geralmente é `requests` grande demais ou afinidade impossível num cluster de tamanho fixo), aqui o cluster *tem* autoscaling de nós configurado mas o Cluster Autoscaler não está subindo um nó novo pra acomodar o pod — motivos comuns: o node group já está no `max size` configurado, o pod pede algo que nenhum tipo de nó no node group oferece (ex: GPU, arquitetura específica), ou uma `nodeAffinity`/taint torna o pod inelegível pra qualquer node group existente.

**Diagnóstico**:
- `kubectl describe pod <pod> -n <ns>` — evento do scheduler já indica insuficiência de recursos em todos os nós atuais
- Log do Cluster Autoscaler (`kubectl logs -n kube-system deploy/cluster-autoscaler --tail=100`) — mostra explicitamente por que ele decidiu (ou não) escalar: `max node group size reached`, `no node group available for pod`, etc.
- Conferir o `max size` configurado do node group relevante (via console do provedor cloud ou `kubectl get nodes -L <label-do-node-group>` pra contar quantos já existem)

**Solução**: se bateu no `max size`, é decisão de capacidade — aumentar o limite (com o time responsável pelo orçamento de infra) ou revisar se o pedido de recurso do pod está superdimensionado. Se o pod pede algo que nenhum node group oferece, ou a `nodeAffinity` é impossível de satisfazer, o problema está na spec do pod, não no autoscaler.

---

## CronJob/Job falhando repetidamente (BackoffLimitExceeded)
kind: JobBackoffLimitExceeded
keywords: job, cronjob, backofflimitexceeded, job has reached the specified backoff limit

**Causa comum**: o `Job` (direto, ou criado por um `CronJob`) tentou rodar `backoffLimit` vezes (padrão 6) e todas as tentativas falharam — o container do Job está saindo com erro, não um problema do Job em si.

**Diagnóstico**:
- `kubectl get jobs -n <ns>` — `FAILED` no lugar de `COMPLETIONS`
- `kubectl get pods -n <ns> -l job-name=<nome-do-job>` — lista todas as tentativas (pods), cada uma é uma tentativa separada
- `kubectl logs -n <ns> -l job-name=<nome-do-job> --tail=50` (ou `--previous` na última tentativa) — log da tentativa mais recente, o motivo real do erro geralmente está aqui

**Solução**: o `Job`/`CronJob` em si nunca precisa de intervenção (ele já parou de tentar sozinho) — o trabalho é depurar por que o container falha, igual qualquer `PodCrashLoopBackOff`. Depois de corrigir a causa raiz, um `Job` que já esgotou o `backoffLimit` não se recupera sozinho: precisa deletar e deixar o próximo `CronJob` criar um novo (ou criar manualmente pra um `Job` avulso).

---

## StatefulSet travado no rollout (réplica não fica Ready)
kind: StatefulSetRolloutStuck
keywords: statefulset, rollout stuck, partition, waiting for pod, pod is not ready

**Causa comum**: `StatefulSet` atualiza réplicas uma de cada vez, em ordem (0, 1, 2, ...), e só avança pra próxima quando a atual fica `Ready` — diferente de `Deployment`, que substitui em paralelo. Se a réplica N não fica `Ready` (crash, falha de probe, PVC que não monta), o rollout inteiro trava ali, mesmo que as réplicas seguintes estivessem saudáveis na versão anterior.

**Diagnóstico**:
- `kubectl rollout status statefulset/<nome> -n <ns>` — mostra em qual réplica está travado
- `kubectl describe pod <nome>-N -n <ns>` (a réplica específica que travou) — causa real geralmente é `PodCrashLoopBackOff`/probe falhando/PVC não montando, os mesmos problemas das entradas específicas acima
- `kubectl get pvc -n <ns> -l <label-do-statefulset>` — confirma se o volume daquela réplica específica está `Bound`

**Solução**: resolver a causa raiz na réplica travada (é sempre um dos problemas já catalogados aqui — crash, probe, PVC). Se for preciso reverter enquanto investiga, `kubectl rollout undo statefulset/<nome> -n <ns>` volta pra versão anterior — mas cuidado: diferente de `Deployment`, isso ainda respeita a ordem/uma-de-cada-vez, não é instantâneo.

---

## Ingress não roteia tráfego (404/502/503 do controller)
kind: IngressNotRouting
keywords: ingress, 404, 502, 503, default backend, ingress controller, no endpoints, host not found

**Causa comum**: o Ingress Controller (nginx-ingress, ALB, Traefik, ...) recebeu a requisição mas não conseguiu rotear pro `Service`/pod certo — `Ingress` apontando pro `Service` errado ou inexistente, `Service` sem endpoints (ver `ServiceNoEndpoints` acima), host/path do `Ingress` não bate com o da requisição, ou o `IngressClass` errado (mais de um controller no cluster).

**Diagnóstico**:
- `kubectl get ingress -n <ns> <nome> -o yaml` — conferir `host`, `path`, `backend.service.name`/`port` batem com o esperado, e o `ingressClassName` é o controller certo
- `kubectl describe ingress <nome> -n <ns>` — eventos costumam apontar erro de sincronização direto
- `kubectl get endpoints <service-do-backend> -n <ns>` — vazio explica um 502/503 direto (mesma causa de `ServiceNoEndpoints`)
- Log do próprio Ingress Controller (`kubectl logs -n <ns-do-controller> <pod-do-controller>`) — mostra a config nginx/rota gerada e qualquer erro de sincronização com a API

**Solução**: depende do que o diagnóstico achou — corrigir `host`/`path`/`backend` do `Ingress` se estiver errado, corrigir o `Service`/seletor de pods se não há endpoints, ou apontar o `ingressClassName` certo se há mais de um controller competindo pela mesma regra.

---

## Certificado TLS expirado (API server, webhook ou serviço interno)
kind: CertificateExpired
keywords: certificate expired, x509, certificate has expired, tls handshake, unknown authority
log_signatures: certificate has expired or is not yet valid, x509: certificate signed by unknown authority
log_source: self

**Causa comum**: um certificado TLS (do próprio kubelet/API server, de um webhook, de um serviço interno usando mTLS, ou emitido pelo cert-manager) expirou ou foi rotacionado sem propagar corretamente pra quem confia nele. Sintoma comum: comunicação entre componentes que funcionava para de funcionar, sem nenhuma mudança de código ou config aparente — só o tempo passou.

**Diagnóstico**:
- A mensagem de erro já identifica o tipo exato: `x509: certificate has expired or is not yet valid` (expirou) ou `x509: certificate signed by unknown authority` (a CA que assinou não é mais confiada, comum depois de rotação de CA)
- `kubectl get certificate -n <ns>` (se usa cert-manager) — coluna `READY`/`Ready:False` e o campo `status.notAfter` mostra a data de validade
- `echo | openssl s_client -connect <host>:<porta> 2>/dev/null | openssl x509 -noout -dates` — confirma validade de um certificado específico direto, sem depender do cert-manager

**Solução**: se é gerenciado por cert-manager, geralmente basta forçar a renovação (`kubectl delete secret <nome-do-secret-tls> -n <ns>`, o cert-manager recria) — mas confirmar antes que a `Issuer`/`ClusterIssuer` que emite está saudável, senão a renovação vai falhar do mesmo jeito. Se é um certificado manual/estático, precisa gerar um novo e substituir o `Secret` referenciado — processo que varia por componente, sem automação genérica possível aqui.

---

## PodDisruptionBudget bloqueando drain/rollout
kind: PodDisruptionBudgetBlocking
keywords: poddisruptionbudget, pdb, cannot evict pod, disruption allowed, eviction

**Causa comum**: um `PodDisruptionBudget` define o mínimo de réplicas que precisam continuar disponíveis durante uma disrupção voluntária (drain de nó, rollout). Se o número de réplicas saudáveis já está no limite mínimo (ou abaixo, por algum outro problema em paralelo), o Kubernetes recusa evictions adicionais — o que trava um `kubectl drain` ou atrasa um rollout até a situação se resolver sozinha ou alguém intervir.

**Diagnóstico**:
- `kubectl get pdb -n <ns>` — colunas `ALLOWED DISRUPTIONS` (0 é o sintoma) e `MIN AVAILABLE`/`MAX UNAVAILABLE` configurados
- `kubectl get pods -n <ns> -l <label-do-pdb>` — quantas réplicas estão de fato `Ready` agora; se está abaixo do normal, o problema real é por que as réplicas não estão saudáveis (provavelmente outra entrada deste catálogo), não o PDB em si
- `kubectl describe pdb <nome> -n <ns>` — mostra os pods atualmente contados como "saudáveis" pro cálculo

**Solução**: se as réplicas estão de fato todas saudáveis e o PDB só está configurado de forma muito restritiva pro tamanho atual do deployment (ex: `minAvailable` igual ao número de réplicas, sobra zero), é uma conversa com o dono do serviço sobre relaxar o PDB. Se réplicas estão faltando por outro problema (crash, pending, etc.), resolver a causa raiz already resolve o bloqueio do PDB como efeito colateral — nunca contornar deletando/reduzindo o PDB sem entender por que ele está impedindo a operação, é exatamente a proteção que ele existe pra fornecer.

---

## CoreDNS crashando por loop de resolução detectado
kind: CoreDNSLoopDetected
keywords: coredns, loop, plugin/loop, forwarding, crashloopbackoff dns
log_signatures: Loop (127.0.0.1
log_source: kube-system/coredns

**Causa comum**: diferente de `ClusterDNSFailure` acima (que é sobre resolução falhando pros clientes), este é o próprio CoreDNS entrando em `CrashLoopBackOff` porque detectou um loop de encaminhamento — geralmente porque o `/etc/resolv.conf` do nó aponta de volta pro próprio CoreDNS (comum em ambientes com `systemd-resolved` mal configurado, ou depois de mudar o DNS upstream do cluster sem ajustar o `Corefile`).

**Diagnóstico**: o plugin `loop` do CoreDNS detecta isso sozinho e mata o processo de propósito (fail-fast, pra não mascarar o problema) — o log é bem explícito: `Loop (127.0.0.1:XXXXX -> :53) detected for zone ".", see https://coredns.io/plugins/loop#troubleshooting`.

**Solução**: normalmente é ajustar o `Corefile` (`kubectl edit configmap coredns -n kube-system`) pra não encaminhar de volta pro próprio cluster, ou corrigir o `/etc/resolv.conf` dos nós (fora do escopo do Kubernetes em si — é configuração do SO/imagem do nó). O link que o próprio CoreDNS retorna no log tem o passo a passo específico pra cada causa comum — vale seguir ele antes de tentar algo genérico, porque o diagnóstico exato varia bastante por ambiente.

---

## ImagePullBackOff por rate limit do registry (não é imagem inexistente)
kind: ImagePullRateLimited
keywords: toomanyrequests, rate limit, pull rate limit exceeded, imagepullbackoff intermitente
log_signatures: toomanyrequests, pull rate limit exceeded
log_source: self

**Causa comum**: diferente do `PodImagePullBackOff` genérico acima (imagem não existe / sem permissão), aqui a imagem existe e a credencial está certa, mas o registry (Docker Hub é o caso clássico, no plano gratuito/anônimo) está recusando o pull por limite de taxa — comum quando muitos nós/pods do cluster puxam a mesma imagem em pouco tempo (ex: rollout grande, ou nó novo entrando e repuxando tudo).

**Diagnóstico**: a mensagem de erro do `ImagePullBackOff` traz o texto do registry diretamente — `toomanyrequests: You have reached your pull rate limit` (Docker Hub) é a assinatura mais comum. Diferente da falha "imagem não existe", isso costuma ser intermitente: tentar de novo minutos depois às vezes já funciona, porque o limite é por janela de tempo.

**Solução**: a correção real é usar um *pull-through cache*/mirror interno (a maioria das empresas já tem um pra isso, ex: ECR com replicação, Harbor, etc.) ou autenticar com uma conta paga do registry em vez de pull anônimo — ambas são mudanças de infraestrutura, não algo pra resolver pod a pod. Como paliativo imediato, esperar a janela de rate limit resetar costuma destravar sozinho.
