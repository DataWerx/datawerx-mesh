package dnsserver

import (
	"errors"
	"net"
	"testing"

	"github.com/go-logr/logr"
	"github.com/miekg/dns"

	mcsv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/multicluster/v1alpha1"
	dwxdns "github.com/DataWerx/datawerx-mesh/pkg/dns"
)

// fakeResolver is a map-backed Resolver keyed by "ns/name".
type fakeResolver map[string]dwxdns.ResolvedService

func (f fakeResolver) LookupClusterSet(ns, name string) (dwxdns.ResolvedService, bool, error) {
	rs, ok := f[ns+"/"+name]
	return rs, ok, nil
}

// errResolver always fails the lookup, modeling a transient backend error.
type errResolver struct{ err error }

func (e errResolver) LookupClusterSet(string, string) (dwxdns.ResolvedService, bool, error) {
	return dwxdns.ResolvedService{}, false, e.err
}

func query(name string, qtype uint16) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	return m
}

func newServer(r Resolver) *Server {
	return &Server{Resolver: r, Log: logr.Discard()}
}

func TestBuildReply_ClusterSetIP_A(t *testing.T) {
	r := fakeResolver{
		"prod/payments": {Type: mcsv1alpha1.ClusterSetIP, IPs: []string{"241.0.0.5"}},
	}
	m := newServer(r).buildReply(query("payments.prod.svc.clusterset.local", dns.TypeA))

	if m.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %d, want success", m.Rcode)
	}
	if len(m.Answer) != 1 {
		t.Fatalf("expected 1 answer, got %d", len(m.Answer))
	}
	a, ok := m.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("answer is %T, want *dns.A", m.Answer[0])
	}
	if !a.A.Equal(net.ParseIP("241.0.0.5")) {
		t.Errorf("A = %s, want 241.0.0.5", a.A)
	}
	if !m.Authoritative {
		t.Error("expected authoritative reply")
	}
}

func TestBuildReply_AAAA(t *testing.T) {
	r := fakeResolver{
		"prod/payments": {Type: mcsv1alpha1.ClusterSetIP, IPs: []string{"fd00::5", "241.0.0.5"}},
	}
	m := newServer(r).buildReply(query("payments.prod.svc.clusterset.local", dns.TypeAAAA))
	if len(m.Answer) != 1 {
		t.Fatalf("expected 1 AAAA answer (v4 filtered out), got %d", len(m.Answer))
	}
	if _, ok := m.Answer[0].(*dns.AAAA); !ok {
		t.Fatalf("answer is %T, want *dns.AAAA", m.Answer[0])
	}
}

func TestBuildReply_AQueryDoesNotReturnV6(t *testing.T) {
	r := fakeResolver{
		"prod/payments": {Type: mcsv1alpha1.ClusterSetIP, IPs: []string{"fd00::5"}},
	}
	m := newServer(r).buildReply(query("payments.prod.svc.clusterset.local", dns.TypeA))
	// In-zone, known name, but no A records: NODATA (success, no answers).
	if m.Rcode != dns.RcodeSuccess {
		t.Errorf("rcode = %d, want success (NODATA)", m.Rcode)
	}
	if len(m.Answer) != 0 {
		t.Errorf("expected no A answers, got %d", len(m.Answer))
	}
}

func TestBuildReply_UnknownServiceNXDOMAIN(t *testing.T) {
	m := newServer(fakeResolver{}).buildReply(query("ghost.prod.svc.clusterset.local", dns.TypeA))
	if m.Rcode != dns.RcodeNameError {
		t.Errorf("rcode = %d, want NXDOMAIN", m.Rcode)
	}
}

func TestBuildReply_LookupErrorServFail(t *testing.T) {
	// A lookup failure must be SERVFAIL, not NXDOMAIN: NXDOMAIN is negatively
	// cached by downstream resolvers and would mask a live service during a
	// transient backend error.
	m := newServer(errResolver{err: errors.New("cache timeout")}).
		buildReply(query("payments.prod.svc.clusterset.local", dns.TypeA))
	if m.Rcode != dns.RcodeServerFailure {
		t.Errorf("rcode = %d, want SERVFAIL", m.Rcode)
	}
}

func TestBuildReply_OutOfZoneRefused(t *testing.T) {
	m := newServer(fakeResolver{}).buildReply(query("example.com", dns.TypeA))
	if m.Rcode != dns.RcodeRefused {
		t.Errorf("rcode = %d, want Refused", m.Rcode)
	}
}

func TestBuildReply_MalformedInZoneNXDOMAIN(t *testing.T) {
	// In the zone but wrong shape (missing namespace label).
	m := newServer(fakeResolver{}).buildReply(query("prod.svc.clusterset.local", dns.TypeA))
	if m.Rcode != dns.RcodeNameError {
		t.Errorf("rcode = %d, want NXDOMAIN", m.Rcode)
	}
}

func TestBuildReply_HeadlessNoIPsNXDOMAIN(t *testing.T) {
	r := fakeResolver{"data/db": {Type: mcsv1alpha1.Headless, IPs: nil}}
	m := newServer(r).buildReply(query("db.data.svc.clusterset.local", dns.TypeA))
	if m.Rcode != dns.RcodeNameError {
		t.Errorf("rcode = %d, want NXDOMAIN for headless with no endpoints", m.Rcode)
	}
}

// captureWriter implements dns.ResponseWriter to capture the written message.
type captureWriter struct{ msg *dns.Msg }

func (c *captureWriter) LocalAddr() net.Addr       { return &net.UDPAddr{} }
func (c *captureWriter) RemoteAddr() net.Addr      { return &net.UDPAddr{} }
func (c *captureWriter) WriteMsg(m *dns.Msg) error { c.msg = m; return nil }
func (c *captureWriter) Write([]byte) (int, error) { return 0, nil }
func (c *captureWriter) Close() error              { return nil }
func (c *captureWriter) TsigStatus() error         { return nil }
func (c *captureWriter) TsigTimersOnly(bool)       {}
func (c *captureWriter) Hijack()                   {}

func TestServeDNS_WritesReply(t *testing.T) {
	r := fakeResolver{"prod/payments": {Type: mcsv1alpha1.ClusterSetIP, IPs: []string{"241.0.0.5"}}}
	w := &captureWriter{}
	newServer(r).serveDNS(w, query("payments.prod.svc.clusterset.local", dns.TypeA))
	if w.msg == nil {
		t.Fatal("no reply written")
	}
	if len(w.msg.Answer) != 1 {
		t.Errorf("expected 1 answer, got %d", len(w.msg.Answer))
	}
}
