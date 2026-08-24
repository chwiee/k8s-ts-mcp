package inventory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPClient_Lookup_Found(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/clusters/mars-prod-1" {
			t.Errorf("path = %q, want /clusters/mars-prod-1", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"aws_account_id":"123456789012","region":"us-east-1","eks_cluster_name":"mars-prod-1"}`))
	}))
	defer srv.Close()

	c := &HTTPClient{BaseURL: srv.URL}
	info, found, err := c.Lookup(context.Background(), "mars-prod-1")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if info.AWSAccountID != "123456789012" || info.Region != "us-east-1" {
		t.Errorf("info = %+v", info)
	}
}

func TestHTTPClient_Lookup_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := &HTTPClient{BaseURL: srv.URL}
	_, found, err := c.Lookup(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if found {
		t.Error("found = true, want false for a 404")
	}
}

func TestHTTPClient_Lookup_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &HTTPClient{BaseURL: srv.URL}
	_, _, err := c.Lookup(context.Background(), "mars-prod-1")
	if err == nil {
		t.Fatal("Lookup: want an error for a 500, got nil")
	}
}
