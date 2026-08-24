package corek8s

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/chwiee/k8s-ts-mcp/internal/execengine"
	"github.com/chwiee/k8s-ts-mcp/internal/k8sclient"
	"github.com/chwiee/k8s-ts-mcp/internal/playbooks"
	"github.com/chwiee/k8s-ts-mcp/internal/policy"
)

// ExecFormatError catches a container whose entrypoint the kernel can't
// execute at all — almost always an image built for the wrong CPU
// architecture (e.g. an arm64 image scheduled on an amd64 node), sometimes
// a corrupt or non-executable entrypoint. There is no remediation a cluster
// operator can safely automate here: someone has to rebuild the image or
// reschedule it onto a node with a matching architecture. This playbook is
// purely diagnostic — restarting the pod cannot help, so it doesn't
// pretend to offer that — but it does check the cluster's real node
// architectures to tell the two possible fixes apart: rebuild (no other
// architecture available) vs. nodeAffinity (one already is).
type ExecFormatError struct{}

func (ExecFormatError) ID() string       { return "core/execformaterror" }
func (ExecFormatError) Resource() string { return "core" }

func (ExecFormatError) Detect(sig playbooks.Signal) bool {
	return sig.Kind == "PodExecFormatError"
}

func (ExecFormatError) Diagnose(ctx context.Context, cli *k8sclient.Client, sig playbooks.Signal) (playbooks.Finding, error) {
	info, err := cli.ContainerTermination(ctx, sig.Namespace, sig.Name)
	if err != nil {
		return playbooks.Finding{}, fmt.Errorf("diagnosing %s/%s: %w", sig.Namespace, sig.Name, err)
	}

	summary := fmt.Sprintf("pod %s/%s não conseguiu executar o entrypoint do container (exit code %d)", sig.Namespace, sig.Name, info.ExitCode)
	switch {
	case strings.Contains(strings.ToLower(info.Message), "exec format error"):
		summary += ": " + info.Message +
			" — sinal clássico de imagem buildada pra arquitetura de CPU errada"
	case info.Message != "":
		summary += ": " + info.Message
	default:
		// Nem todo runtime de container expõe o texto do erro do kernel nos
		// campos que a API do Kubernetes disponibiliza — só o exit code.
		// Sem a mensagem, isso é uma hipótese a confirmar manualmente, não
		// um diagnóstico certo.
		summary += " (o runtime não expôs mensagem — sem log nem texto de erro disponível via API; " +
			"exit code consistente com falha ao executar o entrypoint, ex: exec format error por arquitetura incompatível ou binário corrompido — verifique manualmente com `kubectl describe pod`/`docker inspect` da imagem)"
	}

	// Best-effort: check what architecture actually failed, and whether the
	// cluster has any node with a different one — this is what decides
	// which of the two possible fixes actually applies. Soft-fails (never
	// aborts the diagnosis) since this is a bonus, not the core finding.
	meta := map[string]string{}
	nodeName, err := cli.PodNodeName(ctx, sig.Namespace, sig.Name)
	if err == nil && nodeName != "" {
		if failedArch, err := cli.NodeArchitecture(ctx, nodeName); err == nil && failedArch != "" {
			meta["node"] = nodeName
			meta["failed_arch"] = failedArch
			if archs, err := cli.ClusterArchitectures(ctx); err == nil {
				var other []string
				for a := range archs {
					if a != failedArch {
						other = append(other, a)
					}
				}
				sort.Strings(other)
				if len(other) > 0 {
					meta["other_archs"] = strings.Join(other, ",")
					summary += fmt.Sprintf(". Rodou no nó %s (%s); o cluster também tem nó(s) %s disponível(is) — "+
						"se a imagem foi buildada pra essa outra arquitetura, um nodeAffinity resolveria sem rebuild.",
						nodeName, failedArch, strings.Join(other, ", "))
				} else {
					summary += fmt.Sprintf(". Rodou no nó %s (%s); todos os nós do cluster são dessa mesma arquitetura, "+
						"então não tem como só reagendar — se a imagem foi buildada pra outra arquitetura, precisa rebuildar.",
						nodeName, failedArch)
				}
			}
		}
	}

	return playbooks.Finding{Signal: sig, Summary: summary, Meta: meta}, nil
}

func (ExecFormatError) Ladder(_ *k8sclient.Client, finding playbooks.Finding) execengine.Ladder {
	ns, pod := finding.Signal.Namespace, finding.Signal.Name
	failedArch := finding.Meta["failed_arch"]
	otherArchs := finding.Meta["other_archs"]

	var description string
	switch {
	case otherArchs != "":
		// A node with a different architecture exists — nodeAffinity might
		// fix it without touching the image at all. Phrased as a
		// hypothesis: we know the *cluster* has that architecture
		// available, we don't know the *image* was built for it.
		description = fmt.Sprintf(
			"se a imagem do pod %s/%s foi buildada para %s (não %s), adicione nodeAffinity pra agendar só nesses nós: "+
				"kubectl patch deployment <nome> -n %s --type=json -p "+
				"'[{\"op\":\"add\",\"path\":\"/spec/template/spec/affinity\",\"value\":{\"nodeAffinity\":{\"requiredDuringSchedulingIgnoredDuringExecution\":"+
				"{\"nodeSelectorTerms\":[{\"matchExpressions\":[{\"key\":\"kubernetes.io/arch\",\"operator\":\"In\",\"values\":[%q]}]}]}}}}]'. "+
				"Se a imagem foi buildada pra %s mesmo (e o problema é outro, ex: binário corrompido), isso não resolve — nesse caso vale rebuildar.",
			ns, pod, otherArchs, failedArch, ns, otherArchs, failedArch)
	case failedArch != "":
		description = fmt.Sprintf(
			"nenhum outro nó com arquitetura diferente disponível no cluster (só %s) — rebuilde a imagem pra essa plataforma: "+
				"docker buildx build --platform linux/%s --push", failedArch, failedArch)
	default:
		// Couldn't even determine the failing node's own architecture
		// (soft-failed in Diagnose) — fall back to the generic multi-arch
		// suggestion rather than claiming something we don't know.
		description = fmt.Sprintf(
			"verifique a arquitetura do nó (kubectl get node -o wide) e da imagem do pod %s/%s, e rebuilde com a plataforma certa: "+
				"docker buildx build --platform linux/amd64,linux/arm64 --push", ns, pod)
	}

	return execengine.Ladder{
		PlaybookID: ExecFormatError{}.ID(),
		Actions: []execengine.Action{
			{
				Name:        "corrigir incompatibilidade de arquitetura",
				Description: description,
				Risk:        policy.RiskHigh, // never auto-run — see internal/execengine.Run; nothing here is safe to automate
			},
		},
	}
}
