// Command cluster-agent runs inside one Kubernetes cluster, dials out to the
// k8s-ts-mcp hub over mTLS gRPC, and executes playbooks locally against that
// cluster's own API server. See docs/ARCHITECTURE.md for the full design.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/chwiee/k8s-ts-mcp/internal/agentcore"
	"github.com/chwiee/k8s-ts-mcp/internal/k8sclient"
	"github.com/chwiee/k8s-ts-mcp/internal/playbooks"
	"github.com/chwiee/k8s-ts-mcp/internal/playbooks/calico"
	"github.com/chwiee/k8s-ts-mcp/internal/playbooks/corek8s"
	"github.com/chwiee/k8s-ts-mcp/internal/playbooks/keda"
	"github.com/chwiee/k8s-ts-mcp/internal/tlsutil"
	"github.com/chwiee/k8s-ts-mcp/internal/transport"
)

const version = "0.1.0"

func main() {
	hubAddr := flag.String("hub-addr", envOr("HUB_ADDR", "localhost:7443"), "endereço host:porta do hub-server")
	clusterID := flag.String("cluster-id", envOr("CLUSTER_ID", ""), "identificador deste cluster, registrado no hub")
	kubeconfig := flag.String("kubeconfig", envOr("KUBECONFIG_PATH", ""), "caminho do kubeconfig (vazio = in-cluster config)")
	calicoNS := flag.String("calico-namespace", envOr("CALICO_NAMESPACE", "calico-system"), "namespace do DaemonSet calico-node")
	certFile := flag.String("cert", envOr("AGENT_CERT", ""), "certificado mTLS do agente")
	keyFile := flag.String("key", envOr("AGENT_KEY", ""), "chave privada mTLS do agente")
	caFile := flag.String("ca", envOr("HUB_CA", ""), "CA usada para validar o certificado do hub")
	insecureMode := flag.Bool("insecure", os.Getenv("INSECURE") == "true", "desliga mTLS — só para desenvolvimento local (ex: kind)")
	flag.Parse()

	if *clusterID == "" {
		log.Fatal("cluster-agent: --cluster-id (ou CLUSTER_ID) é obrigatório")
	}

	cli, err := k8sclient.New(*kubeconfig)
	if err != nil {
		log.Fatalf("cluster-agent: construindo cliente kubernetes: %v", err)
	}

	registry := playbooks.NewRegistry(
		corek8s.CrashLoopBackOff{},
		corek8s.OOMKilled{},
		corek8s.ImagePullBackOff{},
		corek8s.ExecFormatError{},
		calico.NodeDegraded{Namespace: *calicoNS},
		keda.ScaledObjectStuck{},
	)

	handler := &agentcore.Handler{Client: cli, Registry: registry}

	creds, err := tlsutil.ClientCredentials(*certFile, *keyFile, *caFile, *insecureMode)
	if err != nil {
		log.Fatalf("cluster-agent: carregando credenciais mTLS: %v", err)
	}

	client := transport.NewClient(*hubAddr, *clusterID, version, creds, handler)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("cluster-agent %s: conectando ao hub %s como cluster_id=%s (insecure=%v)", version, *hubAddr, *clusterID, *insecureMode)
	if err := client.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("cluster-agent: %v", err)
	}
	log.Println("cluster-agent: encerrando...")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
