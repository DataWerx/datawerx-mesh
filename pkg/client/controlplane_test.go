package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
	dwxclient "github.com/DataWerx/datawerx-mesh/pkg/client"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := networkingv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	return s
}

// LocalGitOpsClient - free tier

func TestLocalGitOpsClient_Authenticate(t *testing.T) {
	t.Run("nil reader fails", func(t *testing.T) {
		c := &dwxclient.LocalGitOpsClient{}
		if err := c.Authenticate(context.Background()); err == nil {
			t.Fatal("expected error for nil reader")
		}
	})
	t.Run("with reader succeeds", func(t *testing.T) {
		fc := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		c := dwxclient.NewLocalGitOpsClient(fc)
		if err := c.Authenticate(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("respects cancelled context", func(t *testing.T) {
		fc := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		c := dwxclient.NewLocalGitOpsClient(fc)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := c.Authenticate(ctx); err == nil {
			t.Fatal("expected context cancellation error")
		}
	})
}

func TestLocalGitOpsClient_FetchTopology(t *testing.T) {
	peerA := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "a"},
		Spec: networkingv1alpha1.MeshPeerSpec{
			ClusterID: "cluster-a", PublicKey: "ka", Endpoint: "1.2.3.4:51820",
			PodCIDRs: []string{"10.10.0.0/16"}, ServiceCIDRs: []string{"10.96.0.0/12"},
		},
	}
	peerB := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Spec:       networkingv1alpha1.MeshPeerSpec{ClusterID: "cluster-b", PublicKey: "kb"},
	}
	// A peer mid-deletion with finalizer and deletion timestamp must be skipped.
	now := metav1.Now()
	peerDeleting := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "deleting",
			DeletionTimestamp: &now,
			Finalizers:        []string{"networking.datawerx.io/meshpeer-cleanup"},
		},
		Spec: networkingv1alpha1.MeshPeerSpec{ClusterID: "cluster-gone", PublicKey: "kg"},
	}

	fc := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(peerA, peerB, peerDeleting).
		Build()

	c := dwxclient.NewLocalGitOpsClient(fc)
	got, err := c.FetchTopology(context.Background())
	if err != nil {
		t.Fatalf("FetchTopology: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 peers (deleting one skipped), got %d: %#v", len(got), got)
	}

	byID := map[string]dwxclient.RemotePeerConfig{}
	for _, p := range got {
		byID[p.ClusterID] = p
	}
	a, ok := byID["cluster-a"]
	if !ok {
		t.Fatal("cluster-a missing")
	}
	want := dwxclient.RemotePeerConfig{
		ClusterID: "cluster-a", PublicKey: "ka", Endpoint: "1.2.3.4:51820",
		PodCIDRs: []string{"10.10.0.0/16"}, ServiceCIDRs: []string{"10.96.0.0/12"},
	}
	if !reflect.DeepEqual(a, want) {
		t.Errorf("cluster-a projection = %#v, want %#v", a, want)
	}
	if _, present := byID["cluster-gone"]; present {
		t.Error("deleting peer should have been skipped")
	}
}

func TestRemotePeerConfig_AllowedIPs(t *testing.T) {
	rc := dwxclient.RemotePeerConfig{
		PodCIDRs:     []string{"10.1.0.0/16"},
		ServiceCIDRs: []string{"10.96.0.0/12"},
	}
	want := []string{"10.1.0.0/16", "10.96.0.0/12"}
	if got := rc.AllowedIPs(); !reflect.DeepEqual(got, want) {
		t.Errorf("AllowedIPs() = %v, want %v", got, want)
	}
}

// EnterpriseControlPlaneClient - premium tier

func TestEnterpriseClient_Authenticate(t *testing.T) {
	t.Run("missing token fails", func(t *testing.T) {
		c := dwxclient.NewEnterpriseControlPlaneClient("https://cp.example",
			dwxclient.WithTokenLoader(func() string { return "" }))
		if err := c.Authenticate(context.Background()); err == nil {
			t.Fatal("expected error for missing SSO token")
		}
	})

	t.Run("empty endpoint fails", func(t *testing.T) {
		c := dwxclient.NewEnterpriseControlPlaneClient("",
			dwxclient.WithTokenLoader(func() string { return "tok" }))
		if err := c.Authenticate(context.Background()); err == nil {
			t.Fatal("expected error for empty endpoint")
		}
	})

	t.Run("valid token accepted and injected as bearer", func(t *testing.T) {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/auth/whoami" {
				gotAuth = r.Header.Get("Authorization")
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		c := dwxclient.NewEnterpriseControlPlaneClient(srv.URL,
			dwxclient.WithTokenLoader(func() string { return "secret-token" }))
		if err := c.Authenticate(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotAuth != "Bearer secret-token" {
			t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer secret-token")
		}
	})

	t.Run("rejected token surfaces error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()

		c := dwxclient.NewEnterpriseControlPlaneClient(srv.URL,
			dwxclient.WithTokenLoader(func() string { return "bad" }))
		if err := c.Authenticate(context.Background()); err == nil {
			t.Fatal("expected error for rejected token")
		}
	})
}

func TestEnterpriseClient_FetchTopology(t *testing.T) {
	peers := []dwxclient.RemotePeerConfig{
		{ClusterID: "c1", PublicKey: "k1", Endpoint: "1.1.1.1:51820", PodCIDRs: []string{"10.1.0.0/16"}},
		{ClusterID: "c2", PublicKey: "k2"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/whoami":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/topology":
			if got := r.Header.Get("Authorization"); got != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"revision": "rev-7",
				"peers":    peers,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := dwxclient.NewEnterpriseControlPlaneClient(srv.URL,
		dwxclient.WithTokenLoader(func() string { return "tok" }))

	// FetchTopology should lazily authenticate if not already done.
	got, err := c.FetchTopology(context.Background())
	if err != nil {
		t.Fatalf("FetchTopology: %v", err)
	}
	if !reflect.DeepEqual(got, peers) {
		t.Errorf("topology = %#v, want %#v", got, peers)
	}
}

func TestEnterpriseClient_FetchTopology_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/whoami" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := dwxclient.NewEnterpriseControlPlaneClient(srv.URL,
		dwxclient.WithTokenLoader(func() string { return "tok" }))
	if _, err := c.FetchTopology(context.Background()); err == nil {
		t.Fatal("expected error on 500 topology response")
	}
}
