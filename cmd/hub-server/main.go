// Command hub-server is k8s-ts-mcp's central piece: it runs in the hub
// cluster, accepts every cluster-agent's mTLS gRPC connection, and exposes
// MCP tools (see internal/mcptools) that reason over the whole fleet. See
// docs/ARCHITECTURE.md for the full design.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	goredis "github.com/redis/go-redis/v9"
	"google.golang.org/grpc"

	"github.com/chwiee/k8s-ts-mcp/internal/agentauth"
	"github.com/chwiee/k8s-ts-mcp/internal/audit"
	"github.com/chwiee/k8s-ts-mcp/internal/inventory"
	"github.com/chwiee/k8s-ts-mcp/internal/mcptools"
	"github.com/chwiee/k8s-ts-mcp/internal/playbooks"
	"github.com/chwiee/k8s-ts-mcp/internal/playbooks/calico"
	"github.com/chwiee/k8s-ts-mcp/internal/playbooks/corek8s"
	"github.com/chwiee/k8s-ts-mcp/internal/playbooks/keda"
	"github.com/chwiee/k8s-ts-mcp/internal/policy"
	"github.com/chwiee/k8s-ts-mcp/internal/registry"
	"github.com/chwiee/k8s-ts-mcp/internal/rolecluster"
	"github.com/chwiee/k8s-ts-mcp/internal/runbooks"
	"github.com/chwiee/k8s-ts-mcp/internal/tlsutil"
	"github.com/chwiee/k8s-ts-mcp/internal/transport"
	pb "github.com/chwiee/k8s-ts-mcp/internal/transport/gen/k8sts/v1"
)

func main() {
	grpcAddr := flag.String("grpc-addr", envOr("GRPC_ADDR", ":7443"), "endereço onde ouvir conexões dos cluster-agents")
	httpAddr := flag.String("http-addr", envOr("HTTP_ADDR", ":8443"), "endereço onde servir o MCP (Streamable HTTP)")
	certFile := flag.String("cert", envOr("HUB_CERT", ""), "certificado mTLS do hub")
	keyFile := flag.String("key", envOr("HUB_KEY", ""), "chave privada mTLS do hub")
	caFile := flag.String("ca", envOr("AGENT_CA", ""), "CA usada para validar o certificado dos agentes")
	insecureMode := flag.Bool("insecure", os.Getenv("INSECURE") == "true", "desliga mTLS — só para desenvolvimento local (ex: kind)")
	groupMappingPath := flag.String("group-mapping", envOr("GROUP_MAPPING_PATH", ""), "caminho do YAML grupo-AD -> tier (ver deployments/hub-server/group-mapping.example.yaml)")
	auditPath := flag.String("audit-path", envOr("AUDIT_PATH", "audit.jsonl"), "caminho do arquivo de auditoria (JSONL)")
	executionFlag := flag.Bool("execution-enabled", os.Getenv("EXECUTION_ENABLED") == "true", "flag global: permite executar ações (não só propor)")
	prodClustersCSV := flag.String("prod-clusters", envOr("PROD_CLUSTERS", ""), "lista separada por vírgula de cluster_id considerados prod")
	testCallerGroupsCSV := flag.String("test-caller-groups", envOr("TEST_CALLER_GROUPS", "infra-prod-admins"), "TEMPORÁRIO: grupos AD usados pra toda chamada, até a identidade real do chamador ser propagada")
	redisAddr := flag.String("redis-addr", envOr("REDIS_ADDR", ""), "endereço host:porta do Redis compartilhado entre réplicas (vazio = modo réplica única, registro só em memória — nunca use vazio com mais de 1 réplica)")
	selfAddr := flag.String("self-addr", envOr("SELF_ADDR", ""), "endereço desta réplica, alcançável por outras réplicas (ex: $(POD_IP):7443) — obrigatório se --redis-addr for informado")
	runbooksPath := flag.String("runbooks-path", envOr("RUNBOOKS_PATH", ""), "caminho do markdown de runbooks (ver docs/runbooks/kubernetes-errors.md) — vazio desliga o fallback pra sinais sem playbook automatizado")
	runbooksReloadEvery := flag.Duration("runbooks-reload-interval", 30*time.Second, "intervalo de releitura automática do arquivo de runbooks (pega edição sem reiniciar)")
	calicoNS := flag.String("calico-namespace", envOr("CALICO_NAMESPACE", "calico-system"), "namespace do DaemonSet calico-node, usado pelos clusters via Role (mesmo default do cluster-agent)")
	clusterInventoryPath := flag.String("cluster-inventory-path", envOr("CLUSTER_INVENTORY_PATH", ""), "caminho do YAML de inventário de clusters pra uso local/dev (cluster_id -> aws_account_id/region/eks_cluster_name, ver internal/inventory) — ignorado se --inventory-api-url for informado")
	inventoryAPIURL := flag.String("inventory-api-url", envOr("INVENTORY_API_URL", ""), "URL base da API de inventário de clusters da empresa (ver internal/inventory.HTTPClient) — tem prioridade sobre --cluster-inventory-path quando os dois são informados")
	agentScopesConfigPath := flag.String("agent-scopes-config", envOr("AGENT_SCOPES_CONFIG_PATH", ""), "caminho do YAML de token->contas AWS permitidas por agente chamador (ver internal/agentauth) — vazio desliga a checagem de escopo de conta (modo dev/local)")
	flag.Parse()

	groups, err := loadGroupMapping(*groupMappingPath)
	if err != nil {
		log.Fatalf("hub-server: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	policyEngine, err := policy.NewEngine(ctx, groups)
	if err != nil {
		log.Fatalf("hub-server: construindo policy engine: %v", err)
	}

	auditStore, err := audit.NewFileStore(*auditPath)
	if err != nil {
		log.Fatalf("hub-server: abrindo audit store: %v", err)
	}

	if *redisAddr != "" && *selfAddr == "" {
		log.Fatal("hub-server: --self-addr é obrigatório quando --redis-addr é informado (endereço desta réplica alcançável pelas outras)")
	}

	hub := transport.NewServer()
	if *redisAddr != "" {
		rdb := goredis.NewClient(&goredis.Options{Addr: *redisAddr})
		if err := rdb.Ping(ctx).Err(); err != nil {
			log.Fatalf("hub-server: conectando no Redis %s: %v", *redisAddr, err)
		}
		peerCreds, err := tlsutil.ClientCredentials(*certFile, *keyFile, *caFile, *insecureMode)
		if err != nil {
			log.Fatalf("hub-server: carregando credenciais de peer (réplica-a-réplica): %v", err)
		}
		hub.Registry = registry.NewRedis(rdb)
		hub.SelfAddr = *selfAddr
		hub.PeerCreds = peerCreds
		log.Printf("hub-server: registro compartilhado via Redis %s, self-addr=%s — pronto pra rodar como múltiplas réplicas", *redisAddr, *selfAddr)
	} else {
		log.Printf("hub-server: --redis-addr não informado — registro só em memória, modo réplica única (não escale hub-server > 1 réplica assim)")
	}

	// clusterInventory answers "what AWS account/region is cluster_id in?"
	// — the one thing internal/rolecluster needs to reach a cluster purely
	// by IRSA-role naming convention (no per-cluster registration), and
	// what mcptools uses to enforce AgentScope below. --inventory-api-url
	// (the company's real inventory API) wins when both are set;
	// --cluster-inventory-path (a static YAML) exists for local/dev use,
	// e.g. testing against Floci.
	var clusterInventory inventory.Lookup
	switch {
	case *inventoryAPIURL != "":
		clusterInventory = &inventory.HTTPClient{BaseURL: *inventoryAPIURL}
		log.Printf("hub-server: inventário de clusters via API em %s", *inventoryAPIURL)
	case *clusterInventoryPath != "":
		f, err := os.Open(*clusterInventoryPath)
		if err != nil {
			log.Fatalf("hub-server: abrindo --cluster-inventory-path: %v", err)
		}
		invCfg, err := inventory.LoadConfig(f)
		f.Close()
		if err != nil {
			log.Fatalf("hub-server: lendo --cluster-inventory-path: %v", err)
		}
		clusterInventory = inventory.New(invCfg.Clusters)
		log.Printf("hub-server: %d cluster(s) carregados no inventário local de %s", len(invCfg.Clusters), *clusterInventoryPath)
	default:
		log.Printf("hub-server: nenhum --inventory-api-url nem --cluster-inventory-path informado — sem descoberta de cluster via IAM Role, e list_nodes não terá contexto de conta/região")
	}

	if clusterInventory != nil {
		// Same playbook set cluster-agent registers — a Role-based cluster
		// runs identical diagnose/execute/scan logic, just without a
		// separate agent process or gRPC hop (see internal/rolecluster).
		roleRegistry := playbooks.NewRegistry(
			corek8s.CrashLoopBackOff{},
			corek8s.OOMKilled{},
			corek8s.ImagePullBackOff{},
			corek8s.ExecFormatError{},
			calico.NodeDegraded{Namespace: *calicoNS},
			keda.ScaledObjectStuck{},
		)
		hub.RoleResolver = rolecluster.NewManager(clusterInventory, roleRegistry)
	}

	var agentRegistry *agentauth.Registry
	if *agentScopesConfigPath != "" {
		f, err := os.Open(*agentScopesConfigPath)
		if err != nil {
			log.Fatalf("hub-server: abrindo --agent-scopes-config: %v", err)
		}
		agentCfg, err := agentauth.LoadConfig(f)
		f.Close()
		if err != nil {
			log.Fatalf("hub-server: lendo --agent-scopes-config: %v", err)
		}
		agentRegistry = agentauth.NewRegistry(agentCfg.Agents)
		log.Printf("hub-server: %d agente(s) carregados de %s — escopo de conta AWS por chamador está ATIVO", len(agentCfg.Agents), *agentScopesConfigPath)
	} else {
		log.Printf("hub-server: nenhum --agent-scopes-config informado — sem checagem de escopo de conta por chamador (modo dev/local)")
	}

	var runbooksStore *runbooks.Store
	if *runbooksPath != "" {
		runbooksStore, err = runbooks.NewStore(*runbooksPath)
		if err != nil {
			log.Fatalf("hub-server: carregando runbooks: %v", err)
		}
		log.Printf("hub-server: %d runbook(s) carregados de %s", runbooksStore.Len(), *runbooksPath)
		go runbooksStore.WatchAndReload(ctx, *runbooksReloadEvery, func(err error) {
			log.Printf("hub-server: falha recarregando runbooks (mantendo o conjunto anterior): %v", err)
		})
	} else {
		log.Printf("hub-server: nenhum --runbooks-path informado — sinais sem playbook automatizado não terão proposta de fallback")
	}

	tools := &mcptools.Server{
		Hub:           hub,
		Policy:        policyEngine,
		Audit:         auditStore,
		ExecutionFlag: *executionFlag,
		ProdClusters:  toSet(*prodClustersCSV),
		CallerGroups:  splitCSV(*testCallerGroupsCSV),
		Runbooks:      runbooksStore,
		Inventory:     clusterInventory,
	}

	mcpInstructions := "Ferramentas de troubleshooting preditivo de Kubernetes para a frota de clusters da empresa. " +
			"Use list_clusters se não tiver certeza do cluster_id exato. Se o usuário não souber o nome de um pod " +
			"específico (ex: \"valide os pods do spoke-1\", \"tem algo quebrado nesse cluster?\"), use scan_cluster " +
			"primeiro para descobrir o que está com problema, e só depois chame troubleshoot com o kind/namespace/name " +
			"exatos que scan_cluster retornou — nunca invente um nome de pod. Use troubleshoot para diagnosticar (e, " +
			"se a política permitir, corrigir) um problema já identificado — sempre confirme o cluster_id com o " +
			"usuário antes de chamar, nunca adivinhe. Se troubleshoot retornar dry_run=true (sempre o caso quando " +
			"playbook_id=\"runbook\", ou seja, veio do catálogo de referência em vez de um playbook automatizado), " +
			"explique ao usuário os proposed_commands em vez de dizer que foi corrigido — o usuário precisa aplicar a " +
			"solução manualmente. Se um attempt vier com skipped=true (ação de risco alto), explique a Description " +
			"dela ao usuário e pergunte explicitamente se ele quer aprovar essa ação específica — só chame " +
			"approve_action depois de uma confirmação clara do usuário (\"sim\", \"pode aprovar\", etc.), nunca por " +
			"conta própria; se o usuário não confirmar, não chame. Use get_postmortem para recuperar o resumo de um " +
			"incidente já tratado. Todos os clusters da empresa são clusters cloud (EKS) — em qualquer tool que peça " +
			"cluster_id, se o usuário não informar explicitamente qual cluster (mesmo que ele mencione uma região ou " +
			"conta AWS), pergunte antes de chamar a tool; nunca escolha um cluster sozinho."

	// newSessionServer builds a fresh *mcp.Server per new MCP session (the
	// go-sdk's Streamable HTTP handler calls this once per session, with
	// that session's initiating request — see NewStreamableHTTPHandler's
	// doc comment), resolving AgentScope from the session's Authorization
	// header. This is deliberately NOT one shared *mcp.Server for every
	// caller: AgentScope has to be per-caller, and mcptools.Server is a
	// plain struct (not goroutine-shared mutable state beyond the pointers
	// it holds), so a cheap shallow copy per session is enough — Hub/
	// Policy/Audit/Runbooks/Inventory stay the same shared instances,
	// only AgentScope differs.
	newSessionServer := func(req *http.Request) *mcp.Server {
		sessionTools := *tools
		if agentRegistry != nil {
			token := agentauth.TokenFromHeader(req.Header)
			scope, ok := agentRegistry.Resolve(token)
			if !ok {
				// A hub with --agent-scopes-config configured requires a
				// valid token on every session — a missing or unrecognized
				// one must fail closed (deny every account), never fall
				// back to AgentScope==nil's permissive default, which is
				// reserved for "no --agent-scopes-config at all."
				log.Printf("hub-server: sessão MCP sem token de agente reconhecido — negando acesso a qualquer conta")
				scope = agentauth.DenyAllScope
			}
			sessionTools.AgentScope = scope
		}

		srv := mcp.NewServer(&mcp.Implementation{
			Name:    "k8s-ts-mcp",
			Version: "0.1.0",
		}, &mcp.ServerOptions{Instructions: mcpInstructions})
		mcptools.Register(srv, &sessionTools)
		return srv
	}

	grpcCreds, err := tlsutil.ServerCredentials(*certFile, *keyFile, *caFile, *insecureMode)
	if err != nil {
		log.Fatalf("hub-server: carregando credenciais mTLS: %v", err)
	}
	grpcServer := grpc.NewServer(grpc.Creds(grpcCreds))
	pb.RegisterAgentServiceServer(grpcServer, hub)
	pb.RegisterInternalServiceServer(grpcServer, hub.InternalHandler())

	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Fatalf("hub-server: escutando gRPC em %s: %v", *grpcAddr, err)
	}
	go func() {
		log.Printf("hub-server: gRPC (agentes) ouvindo em %s (insecure=%v)", *grpcAddr, *insecureMode)
		if err := grpcServer.Serve(lis); err != nil {
			log.Printf("hub-server: gRPC server encerrou: %v", err)
		}
	}()

	httpHandler := mcp.NewStreamableHTTPHandler(newSessionServer, nil)
	httpServer := &http.Server{Addr: *httpAddr, Handler: httpHandler}
	go func() {
		log.Printf("hub-server: MCP (Streamable HTTP) ouvindo em %s", *httpAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("hub-server: HTTP server encerrou: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("hub-server: encerrando...")
	grpcServer.GracefulStop()
	_ = httpServer.Close()
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func toSet(csv string) map[string]bool {
	out := make(map[string]bool)
	for _, id := range splitCSV(csv) {
		out[id] = true
	}
	return out
}

func loadGroupMapping(path string) (policy.GroupMapping, error) {
	if path == "" {
		log.Printf("hub-server: nenhum --group-mapping informado, usando mapeamento padrão de teste (infra-prod-admins -> prod-admin)")
		return policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return policy.LoadGroupMapping(f)
}
