package k8sclient

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/aws-iam-authenticator/pkg/token"
)

// NewFromAWSRole builds a Client for a cluster reached by assuming an AWS
// IAM Role and talking to its EKS API server directly, instead of running a
// cluster-agent inside it — the alternative connection model documented in
// docs/ARCHITECTURE.md's Role-based section. roleARN is assumed via STS
// (from the hub's own ambient AWS identity — instance profile, IRSA, or
// AWS_* env vars, whatever the process already has); eksClusterName/region
// identify the EKS cluster to call DescribeCluster on for its endpoint and
// CA. roleARN's trust policy must trust that ambient identity.
//
// Authentication to the Kubernetes API itself uses the same token format
// `aws eks get-token --role-arn`/aws-iam-authenticator produces (a
// presigned STS GetCallerIdentity URL, base64 with a "k8s-aws-v1." prefix,
// signed AS the assumed role) — generated fresh on every request via a
// custom RoundTripper, since these tokens are short-lived (~15min).
// Validated locally against Floci (a LocalStack-compatible AWS emulator
// that actually runs a k3s container per emulated EKS cluster) before
// being wired in here.
func NewFromAWSRole(ctx context.Context, roleARN, eksClusterName, region string) (*Client, error) {
	baseCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	// Every AWS call from here on — DescribeCluster included — runs as
	// roleARN, not the hub's own base identity: in a real multi-account
	// setup the hub's base role has no standing permissions in the
	// cluster's account at all, only whatever roleARN's trust policy and
	// attached policies grant.
	assumed := aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(sts.NewFromConfig(baseCfg), roleARN, func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = "k8s-ts-mcp-hub"
	}))
	roleCfg := baseCfg.Copy()
	roleCfg.Credentials = assumed

	eksCli := eks.NewFromConfig(roleCfg)
	descOut, err := eksCli.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(eksClusterName)})
	if err != nil {
		return nil, fmt.Errorf("describing EKS cluster %s (as %s): %w", eksClusterName, roleARN, err)
	}
	cluster := descOut.Cluster
	if cluster.Endpoint == nil || cluster.CertificateAuthority == nil {
		return nil, fmt.Errorf("EKS cluster %s has no endpoint/CA yet (status=%s) — not ready", eksClusterName, cluster.Status)
	}
	caPEM, err := base64.StdEncoding.DecodeString(aws.ToString(cluster.CertificateAuthority.Data))
	if err != nil {
		return nil, fmt.Errorf("decoding CA for EKS cluster %s: %w", eksClusterName, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("EKS cluster %s: CertificateAuthority data isn't a valid PEM bundle", eksClusterName)
	}

	gen, err := token.NewGenerator(false, false)
	if err != nil {
		return nil, fmt.Errorf("building IAM authenticator token generator: %w", err)
	}
	src := &roleTokenSource{
		gen:            gen,
		eksClusterName: eksClusterName,
		region:         region,
		roleARN:        roleARN,
	}

	restCfg := &rest.Config{
		Host: aws.ToString(cluster.Endpoint),
		TLSClientConfig: rest.TLSClientConfig{
			CAData: caPEM,
		},
		WrapTransport: func(rt http.RoundTripper) http.RoundTripper {
			return &bearerRoundTripper{src: src, base: rt}
		},
	}

	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("building clientset for EKS cluster %s: %w", eksClusterName, err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("building dynamic client for EKS cluster %s: %w", eksClusterName, err)
	}
	return &Client{Clientset: cs, Dynamic: dyn}, nil
}

// roleTokenSource generates a fresh IAM-authenticator token — signed as
// roleARN, via GetTokenOptions.AssumeRoleARN, which does its own internal
// AssumeRole from ambient credentials rather than reusing a credential
// object we'd hold onto — on demand, caching it until shortly before it
// expires (the presigned URL a token wraps is valid ~15 minutes). One
// instance is shared by every request the resulting Client ever makes.
type roleTokenSource struct {
	gen            token.Generator
	eksClusterName string
	region         string
	roleARN        string

	mu  sync.Mutex
	cur token.Token
}

// earlyRefresh renews the token this long before its real expiry, so a
// request that starts an instant before expiration doesn't race a cluster
// that's already stopped honoring it.
const earlyRefresh = time.Minute

func (s *roleTokenSource) current(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur.Token != "" && time.Now().Before(s.cur.Expiration.Add(-earlyRefresh)) {
		return s.cur.Token, nil
	}
	tok, err := s.gen.GetWithOptions(ctx, &token.GetTokenOptions{
		Region:        s.region,
		ClusterID:     s.eksClusterName,
		AssumeRoleARN: s.roleARN,
		SessionName:   "k8s-ts-mcp-hub",
	})
	if err != nil {
		return "", fmt.Errorf("generating IAM authenticator token for %s (as %s): %w", s.eksClusterName, s.roleARN, err)
	}
	s.cur = tok
	return tok.Token, nil
}

// bearerRoundTripper injects a fresh Authorization header on every request
// — client-go builds its http.Client once (via WrapTransport) and reuses
// it, so a static bearer token baked into rest.Config would go stale after
// ~15 minutes; this refreshes lazily instead of needing a background timer
// or rebuilding the Client.
type bearerRoundTripper struct {
	src  *roleTokenSource
	base http.RoundTripper
}

func (rt *bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := rt.src.current(req.Context())
	if err != nil {
		return nil, err
	}
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+tok)
	return rt.base.RoundTrip(req)
}
