// Package tlsutil builds the gRPC transport credentials for the hub<->agent
// mTLS channel. Both sides support an explicit "insecure" mode, for local
// development (e.g. against kind clusters) where no CA is set up yet — it
// must be opted into, never a silent fallback when certs are missing.
package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// ClientCredentials builds mTLS credentials for a cluster-agent dialing the
// hub: presents (certFile, keyFile) as its client identity, and trusts the
// hub's server certificate only if it chains to caFile.
func ClientCredentials(certFile, keyFile, caFile string, insecureMode bool) (credentials.TransportCredentials, error) {
	if insecureMode {
		return insecure.NewCredentials(), nil
	}
	cert, pool, err := loadCertAndCA(certFile, keyFile, caFile)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
	}), nil
}

// ServerCredentials builds mTLS credentials for the hub: presents
// (certFile, keyFile) as its server identity, and requires every connecting
// agent to present a client certificate that chains to caFile.
func ServerCredentials(certFile, keyFile, caFile string, insecureMode bool) (credentials.TransportCredentials, error) {
	if insecureMode {
		return insecure.NewCredentials(), nil
	}
	cert, pool, err := loadCertAndCA(certFile, keyFile, caFile)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}), nil
}

func loadCertAndCA(certFile, keyFile, caFile string) (tls.Certificate, *x509.CertPool, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("loading cert/key pair (%s, %s): %w", certFile, keyFile, err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("reading CA file %s: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return tls.Certificate{}, nil, fmt.Errorf("no valid certificates found in CA file %s", caFile)
	}
	return cert, pool, nil
}
