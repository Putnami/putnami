package grpc

import (
	"context"
	"net"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

func TestConfig_KeepaliveServerParameters_Defaults(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "secure-defaults", "keepalive-server-parameter-defaults")
	got := Config{}.keepaliveServerParameters()

	if got.MaxConnectionIdle != defaultMaxConnectionIdle {
		t.Errorf("MaxConnectionIdle = %s, want %s", got.MaxConnectionIdle, defaultMaxConnectionIdle)
	}
	if got.MaxConnectionAge != defaultMaxConnectionAge {
		t.Errorf("MaxConnectionAge = %s, want %s", got.MaxConnectionAge, defaultMaxConnectionAge)
	}
	if got.MaxConnectionAgeGrace != defaultMaxConnectionAgeGrace {
		t.Errorf("MaxConnectionAgeGrace = %s, want %s", got.MaxConnectionAgeGrace, defaultMaxConnectionAgeGrace)
	}
	if got.Time != defaultKeepaliveTime {
		t.Errorf("Time = %s, want %s", got.Time, defaultKeepaliveTime)
	}
	if got.Timeout != defaultKeepaliveTimeout {
		t.Errorf("Timeout = %s, want %s", got.Timeout, defaultKeepaliveTimeout)
	}
}

func TestConfig_KeepaliveServerParameters_Overrides(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "secure-defaults", "keepalive-parameters-are-overridable")
	cfg := Config{
		MaxConnectionIdle:     1 * time.Minute,
		MaxConnectionAge:      2 * time.Minute,
		MaxConnectionAgeGrace: 3 * time.Second,
		KeepaliveTime:         4 * time.Second,
		KeepaliveTimeout:      5 * time.Second,
	}
	got := cfg.keepaliveServerParameters()

	if got.MaxConnectionIdle != cfg.MaxConnectionIdle {
		t.Errorf("MaxConnectionIdle = %s, want %s", got.MaxConnectionIdle, cfg.MaxConnectionIdle)
	}
	if got.MaxConnectionAge != cfg.MaxConnectionAge {
		t.Errorf("MaxConnectionAge = %s, want %s", got.MaxConnectionAge, cfg.MaxConnectionAge)
	}
	if got.MaxConnectionAgeGrace != cfg.MaxConnectionAgeGrace {
		t.Errorf("MaxConnectionAgeGrace = %s, want %s", got.MaxConnectionAgeGrace, cfg.MaxConnectionAgeGrace)
	}
	if got.Time != cfg.KeepaliveTime {
		t.Errorf("Time = %s, want %s", got.Time, cfg.KeepaliveTime)
	}
	if got.Timeout != cfg.KeepaliveTimeout {
		t.Errorf("Timeout = %s, want %s", got.Timeout, cfg.KeepaliveTimeout)
	}
}

func TestConfig_EnforcementPolicy(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "secure-defaults", "keepalive-enforcement-policy-is-applied")
	def := Config{}.keepaliveEnforcementPolicy()
	if def.MinTime != defaultMinClientPingInterval {
		t.Errorf("default MinTime = %s, want %s", def.MinTime, defaultMinClientPingInterval)
	}
	if !def.PermitWithoutStream {
		t.Error("PermitWithoutStream should be true so idle-connection keepalives are not penalized")
	}

	custom := Config{MinClientPingInterval: 90 * time.Second}.keepaliveEnforcementPolicy()
	if custom.MinTime != 90*time.Second {
		t.Errorf("custom MinTime = %s, want 90s", custom.MinTime)
	}
}

func TestConfig_MaxConcurrentStreams(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "secure-defaults", "max-concurrent-streams-is-applied")
	if got := (Config{}).maxConcurrentStreams(); got != defaultMaxConcurrentStreams {
		t.Errorf("default = %d, want %d", got, defaultMaxConcurrentStreams)
	}
	if got := (Config{MaxConcurrentStreams: 42}).maxConcurrentStreams(); got != 42 {
		t.Errorf("override = %d, want 42", got)
	}
}

// TestStart_AppliesKeepaliveConfigWithoutError is a smoke test that the
// keepalive/stream options are accepted by grpc.NewServer when a custom
// Config is supplied — the server must start and stop cleanly.
func TestStart_AppliesKeepaliveConfigWithoutError(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "secure-defaults", "keepalive-config-applies-at-start")
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	p := NewPlugin(Config{
		Port:                 port,
		MaxConnectionAge:     1 * time.Minute,
		MaxConcurrentStreams: 50,
	})
	if err := p.Start(context.Background(), nil); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	if err := p.Stop(context.Background(), nil); err != nil {
		t.Fatalf("stop failed: %v", err)
	}
}
