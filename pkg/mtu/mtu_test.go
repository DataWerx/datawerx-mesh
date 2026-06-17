package mtu_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/go-logr/logr"

	"github.com/DataWerx/datawerx-mesh/pkg/mtu"
)

func TestBuildClampRules(t *testing.T) {
	rules := mtu.BuildClampRules("dwx-mesh0")
	want := []mtu.Rule{{
		Chain: mtu.MSSChain,
		Args: []string{
			"-o", "dwx-mesh0",
			"-p", "tcp", "--tcp-flags", "SYN,RST", "SYN",
			"-j", "TCPMSS", "--clamp-mss-to-pmtu",
		},
	}}
	if !reflect.DeepEqual(rules, want) {
		t.Errorf("rules = %v, want %v", rules, want)
	}
}

func TestBuildClampRules_EmptyIface(t *testing.T) {
	if rules := mtu.BuildClampRules(""); len(rules) != 0 {
		t.Errorf("empty iface should yield no rules, got %v", rules)
	}
}

type fakePlane struct {
	iface string
	calls int
	err   error
}

func (f *fakePlane) EnsureClamp(iface string) error {
	f.iface = iface
	f.calls++
	return f.err
}

func TestEnsurer_StartEnsuresImmediately(t *testing.T) {
	fp := &fakePlane{}
	e := &mtu.Ensurer{Iface: "dwx-mesh0", Plane: fp, Log: logr.Discard()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled: ensure once, then return
	if err := e.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if fp.calls != 1 || fp.iface != "dwx-mesh0" {
		t.Errorf("expected one ensure for dwx-mesh0, got calls=%d iface=%q", fp.calls, fp.iface)
	}
}

func TestEnsurer_StartRequiresConfig(t *testing.T) {
	if err := (&mtu.Ensurer{Plane: &fakePlane{}, Log: logr.Discard()}).Start(context.Background()); err == nil {
		t.Error("expected error with no iface")
	}
	if err := (&mtu.Ensurer{Iface: "x", Log: logr.Discard()}).Start(context.Background()); err == nil {
		t.Error("expected error with no plane")
	}
}

func TestEnsurer_ToleratesPlaneError(t *testing.T) {
	fp := &fakePlane{err: errors.New("iptables boom")}
	e := &mtu.Ensurer{Iface: "dwx-mesh0", Plane: fp, Log: logr.Discard()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.Start(ctx); err != nil {
		t.Fatalf("a transient ensure error must not be fatal: %v", err)
	}
	if fp.calls != 1 {
		t.Errorf("expected the ensure to be attempted, got %d", fp.calls)
	}
}

func TestEnsurer_NeedLeaderElection(t *testing.T) {
	if (&mtu.Ensurer{}).NeedLeaderElection() {
		t.Error("MSS clamp must run on every node")
	}
}
