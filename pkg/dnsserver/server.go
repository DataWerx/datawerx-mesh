// Package dnsserver serves the clusterset.local DNS zone from ServiceImport
// objects, completing the cross-cluster discovery pipeline: a pod's resolver
// (via the cluster CoreDNS, which forwards the clusterset.local zone here)
// asks for "<svc>.<ns>.svc.clusterset.local" and receives the imported
// service's ClusterSetIP (or, for headless services, its endpoint IPs).
//
// The server is a thin shell: all name parsing lives in the pure pkg/dns
// helpers, and the answer data comes from a Resolver (cache-backed in
// production, faked in tests). It runs as a controller-runtime Runnable on
// every agent pod and is fronted by a Service that CoreDNS forwards to.
package dnsserver

import (
	"context"
	"net"
	"time"

	"github.com/go-logr/logr"
	"github.com/miekg/dns"

	dwxdns "github.com/datawerx/datawerx/pkg/dns"
	dwxmetrics "github.com/datawerx/datawerx/pkg/metrics"
)

// defaultTTL is the record TTL in seconds. Kept short so imported-service
// changes propagate quickly.
const defaultTTL uint32 = 5

// DefaultBindAddress is the default listen address for the zone server.
const DefaultBindAddress = ":5353"

// Resolver supplies the answer data for a clusterset.local name. It is the seam
// that decouples the wire-protocol handler from how ServiceImports are read.
type Resolver interface {
	// LookupClusterSet returns the imported service for namespace/name.
	//   - found=false, err=nil  → the service is genuinely not imported (NXDOMAIN)
	//   - err != nil            → the lookup itself failed (SERVFAIL); the caller
	//                             must NOT answer NXDOMAIN, which would be
	//                             negatively cached and mask a transient failure.
	LookupClusterSet(namespace, name string) (dwxdns.ResolvedService, bool, error)
}

// Server answers the clusterset.local zone over UDP and TCP.
type Server struct {
	// Addr is the listen address (host:port). Defaults to DefaultBindAddress.
	Addr string
	// Resolver supplies answer data.
	Resolver Resolver
	// Log is optional.
	Log logr.Logger
	// TTL overrides the record TTL when non-zero.
	TTL uint32

	udp *dns.Server
	tcp *dns.Server
}

// NeedLeaderElection makes the server run on every pod even when leader election
// is enabled: each node's CoreDNS forwards to its local responder.
func (s *Server) NeedLeaderElection() bool { return false }

// Start runs the UDP and TCP listeners until the context is cancelled,
// satisfying manager.Runnable.
func (s *Server) Start(ctx context.Context) error {
	addr := s.Addr
	if addr == "" {
		addr = DefaultBindAddress
	}
	handler := dns.HandlerFunc(s.serveDNS)
	s.udp = &dns.Server{Addr: addr, Net: "udp", Handler: handler}
	s.tcp = &dns.Server{Addr: addr, Net: "tcp", Handler: handler}

	errc := make(chan error, 2)
	go func() { errc <- s.udp.ListenAndServe() }()
	go func() { errc <- s.tcp.ListenAndServe() }()
	s.Log.Info("clusterset.local DNS server listening", "addr", addr)

	shutdown := func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.udp.ShutdownContext(shutCtx)
		_ = s.tcp.ShutdownContext(shutCtx)
	}

	select {
	case <-ctx.Done():
		shutdown()
		return nil
	case err := <-errc:
		// One listener failed; tear the other down too rather than leak it.
		shutdown()
		return err
	}
}

// serveDNS handles a single query. Exported indirectly via Start; separated for
// unit testing with a fake ResponseWriter.
func (s *Server) serveDNS(w dns.ResponseWriter, r *dns.Msg) {
	start := time.Now()
	m := s.buildReply(r)
	dwxmetrics.DNSQueries.WithLabelValues(dns.RcodeToString[m.Rcode]).Inc()
	dwxmetrics.DNSQueryDuration.Observe(time.Since(start).Seconds())
	_ = w.WriteMsg(m)
}

// buildReply computes the response message for a query. It is pure with respect
// to the Resolver, which makes it directly testable.
func (s *Server) buildReply(r *dns.Msg) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true

	if len(r.Question) != 1 {
		m.Rcode = dns.RcodeRefused
		return m
	}
	q := r.Question[0]

	// Only the clusterset.local zone is ours.
	if !dwxdns.InClusterSetZone(q.Name) {
		m.Rcode = dns.RcodeRefused
		return m
	}

	name, ns, ok := dwxdns.ParseClusterSetName(q.Name)
	if !ok {
		m.Rcode = dns.RcodeNameError // NXDOMAIN: in-zone but not a service name.
		return m
	}

	resolved, found, err := s.Resolver.LookupClusterSet(ns, name)
	if err != nil {
		// A lookup failure (e.g. cache timeout, transient API error) is a server-side
		// problem, not proof the name is absent. Answer SERVFAIL so downstream
		// resolvers retry instead of negatively caching an NXDOMAIN.
		m.Rcode = dns.RcodeServerFailure
		return m
	}
	if !found || len(resolved.IPs) == 0 {
		m.Rcode = dns.RcodeNameError
		return m
	}

	s.appendAnswers(m, q, resolved.IPs)
	return m
}

// appendAnswers fills m.Answer with the A or AAAA records matching the query
// type. An in-zone, known-name query for any other type yields NODATA (NOERROR
// with no answers), which is the correct response.
func (s *Server) appendAnswers(m *dns.Msg, q dns.Question, ips []string) {
	switch q.Qtype {
	case dns.TypeA:
		for _, ip := range ips {
			if v4 := net.ParseIP(ip).To4(); v4 != nil {
				m.Answer = append(m.Answer, &dns.A{Hdr: s.header(q.Name, dns.TypeA), A: v4})
			}
		}
	case dns.TypeAAAA:
		for _, ip := range ips {
			parsed := net.ParseIP(ip)
			if parsed != nil && parsed.To4() == nil {
				m.Answer = append(m.Answer, &dns.AAAA{Hdr: s.header(q.Name, dns.TypeAAAA), AAAA: parsed})
			}
		}
	}
}

func (s *Server) header(name string, qtype uint16) dns.RR_Header {
	ttl := s.TTL
	if ttl == 0 {
		ttl = defaultTTL
	}
	return dns.RR_Header{Name: name, Rrtype: qtype, Class: dns.ClassINET, Ttl: ttl}
}
