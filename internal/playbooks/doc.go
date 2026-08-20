// Package playbooks defines the diagnostic/remediation plugin interface
// (core Kubernetes, Calico, KEDA, ...). Playbook bundles themselves are
// versioned OCI artifacts published to ECR and delivered to clusters via
// ArgoCD, not compiled into this binary.
package playbooks
