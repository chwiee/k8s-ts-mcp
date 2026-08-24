// Package keda implements playbooks for KEDA ScaledObjects.
package keda

import (
	"context"
	"fmt"
	"time"

	"github.com/chwiee/k8s-ts-mcp/internal/execengine"
	"github.com/chwiee/k8s-ts-mcp/internal/k8sclient"
	"github.com/chwiee/k8s-ts-mcp/internal/playbooks"
	"github.com/chwiee/k8s-ts-mcp/internal/policy"
)

// ScaledObjectStuck fixes a ScaledObject stuck in a non-Ready state by
// deleting the HPA KEDA generated for it — KEDA owns that HPA and recreates
// it on its next reconcile loop, which is the commonly documented fix.
type ScaledObjectStuck struct{}

func (ScaledObjectStuck) ID() string       { return "keda/scaledobject-stuck" }
func (ScaledObjectStuck) Resource() string { return "keda" }

func (ScaledObjectStuck) Detect(sig playbooks.Signal) bool {
	return sig.Kind == "KEDAScaledObjectStuck"
}

func (ScaledObjectStuck) Diagnose(ctx context.Context, cli *k8sclient.Client, sig playbooks.Signal) (playbooks.Finding, error) {
	ready, msg, err := cli.ScaledObjectReady(ctx, sig.Namespace, sig.Name)
	if err != nil {
		return playbooks.Finding{}, fmt.Errorf("diagnosing scaledobject %s/%s: %w", sig.Namespace, sig.Name, err)
	}
	summary := fmt.Sprintf("scaledobject %s/%s ready=%v: %s", sig.Namespace, sig.Name, ready, msg)
	// KEDA's default HPA name; if this team customized
	// spec.advanced.horizontalPodAutoscalerConfig.name on their ScaledObjects,
	// override this convention here.
	meta := map[string]string{"hpa": "keda-hpa-" + sig.Name}
	return playbooks.Finding{Signal: sig, Summary: summary, Meta: meta}, nil
}

func (ScaledObjectStuck) Ladder(cli *k8sclient.Client, finding playbooks.Finding) execengine.Ladder {
	ns, name := finding.Signal.Namespace, finding.Signal.Name
	hpa := finding.Meta["hpa"]
	return execengine.Ladder{
		PlaybookID: ScaledObjectStuck{}.ID(),
		Actions: []execengine.Action{
			{
				Name:        "delete stuck HPA to force KEDA to recreate it",
				Description: fmt.Sprintf("kubectl delete hpa %s -n %s", hpa, ns),
				Risk:        policy.RiskMedium,
				Run: func(ctx context.Context) (string, error) {
					return cli.DeleteHPA(ctx, ns, hpa)
				},
				Validate: func(ctx context.Context) (bool, error) {
					deadline := time.Now().Add(90 * time.Second)
					for {
						ready, _, err := cli.ScaledObjectReady(ctx, ns, name)
						if err != nil {
							return false, err
						}
						if ready {
							return true, nil
						}
						if time.Now().After(deadline) {
							return false, nil
						}
						select {
						case <-ctx.Done():
							return false, ctx.Err()
						case <-time.After(2 * time.Second):
						}
					}
				},
			},
		},
	}
}
