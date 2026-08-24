package corek8s

import (
	"context"
	"fmt"

	"github.com/chwiee/k8s-ts-mcp/internal/execengine"
	"github.com/chwiee/k8s-ts-mcp/internal/k8sclient"
	"github.com/chwiee/k8s-ts-mcp/internal/playbooks"
)

// ImagePullBackOff only ever offers a retry via restart — there's no safe
// automatic fix for an image that genuinely can't be pulled (wrong tag,
// missing registry credentials, image never pushed). The diagnostic
// message (the registry's own error, e.g. "not found" vs "unauthorized")
// is what actually tells a human what to fix; a retry only helps the rare
// case where it was transient (registry rate limiting, a blip).
type ImagePullBackOff struct{}

func (ImagePullBackOff) ID() string       { return "core/imagepullbackoff" }
func (ImagePullBackOff) Resource() string { return "core" }

func (ImagePullBackOff) Detect(sig playbooks.Signal) bool {
	return sig.Kind == "PodImagePullBackOff"
}

func (ImagePullBackOff) Diagnose(ctx context.Context, cli *k8sclient.Client, sig playbooks.Signal) (playbooks.Finding, error) {
	info, err := cli.ContainerWaiting(ctx, sig.Namespace, sig.Name)
	if err != nil {
		return playbooks.Finding{}, fmt.Errorf("diagnosing %s/%s: %w", sig.Namespace, sig.Name, err)
	}

	summary := fmt.Sprintf("pod %s/%s não consegue baixar a imagem (%s): %s", sig.Namespace, sig.Name, info.Reason, info.Message)
	meta := map[string]string{}
	if dep, err := cli.OwningDeploymentName(ctx, sig.Namespace, sig.Name); err == nil {
		meta["deployment"] = dep
	}
	return playbooks.Finding{Signal: sig, Summary: summary, Meta: meta}, nil
}

func (ImagePullBackOff) Ladder(cli *k8sclient.Client, finding playbooks.Finding) execengine.Ladder {
	ns, pod := finding.Signal.Namespace, finding.Signal.Name
	dep, hasDep := finding.Meta["deployment"]
	hasDep = hasDep && dep != ""

	return execengine.Ladder{
		PlaybookID: ImagePullBackOff{}.ID(),
		Actions:    []execengine.Action{restartPodAction(cli, ns, pod, dep, hasDep)},
	}
}
