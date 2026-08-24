package inventory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// HTTPClient implements Lookup against the company's real cluster inventory
// API.
//
// ASSUMED CONTRACT — not yet confirmed with the team that owns that API.
// This is deliberately isolated to one file behind the Lookup interface so
// swapping in the real shape (path, auth, response fields) touches nothing
// else in this codebase:
//
//	GET {BaseURL}/clusters/{cluster_id}
//	200 OK  -> {"aws_account_id": "...", "region": "...", "eks_cluster_name": "..."}
//	404     -> cluster unknown (Lookup returns found=false, err=nil)
//	anything else -> err
type HTTPClient struct {
	BaseURL string
	// Client is the underlying http.Client. Nil uses http.DefaultClient.
	Client *http.Client
}

func (c *HTTPClient) httpClient() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return http.DefaultClient
}

type httpClusterInfo struct {
	AWSAccountID   string `json:"aws_account_id"`
	Region         string `json:"region"`
	EKSClusterName string `json:"eks_cluster_name"`
}

// Lookup implements Lookup.
func (c *HTTPClient) Lookup(ctx context.Context, clusterID string) (ClusterInfo, bool, error) {
	u := strings.TrimRight(c.BaseURL, "/") + "/clusters/" + url.PathEscape(clusterID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ClusterInfo{}, false, fmt.Errorf("building inventory API request for %s: %w", clusterID, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return ClusterInfo{}, false, fmt.Errorf("calling inventory API for %s: %w", clusterID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ClusterInfo{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return ClusterInfo{}, false, fmt.Errorf("inventory API returned %s for cluster %s", resp.Status, clusterID)
	}

	var body httpClusterInfo
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ClusterInfo{}, false, fmt.Errorf("decoding inventory API response for %s: %w", clusterID, err)
	}
	return ClusterInfo{
		ClusterID:      clusterID,
		AWSAccountID:   body.AWSAccountID,
		Region:         body.Region,
		EKSClusterName: body.EKSClusterName,
	}, true, nil
}

var _ Lookup = (*HTTPClient)(nil)
