package serviceauth

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corepb "github.com/11bronx11/bx_yunpan/backend/internal/core/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Opt-in deployment probe. It clones only the explicitly named TEST container
// configuration into a temporary sibling, with registration disabled. Original
// roles, ports, certificates and deploy/.env are untouched.
func testDeployedCoreTLS(t *testing.T, source, dir string, server, client, expired, wrongCA, root TLS) {
	t.Helper()
	if !strings.HasPrefix(source, "bx-ai-split-smoke-") {
		t.Fatal("explicit dedicated test container required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	command := func(args ...string) []byte {
		t.Helper()
		b, err := exec.CommandContext(ctx, "docker", args...).Output()
		if err != nil {
			t.Fatal("test Docker operation failed", args[0])
		}
		return b
	}
	var cs []struct {
		Image           string
		Config          struct{ Env []string }
		NetworkSettings struct{ Networks map[string]json.RawMessage }
	}
	if json.Unmarshal(command("inspect", source), &cs) != nil || len(cs) != 1 {
		t.Fatal("test source unavailable")
	}
	env := map[string]string{}
	for _, kv := range cs[0].Config.Env {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			env[k] = v
		}
	}
	env["ETCD_ENABLED"] = "false"
	env["INTERNAL_GRPC_TLS_CA"] = "/certs/" + filepath.Base(root.Cert)
	env["INTERNAL_GRPC_TLS_CERT"] = "/certs/" + filepath.Base(server.Cert)
	env["INTERNAL_GRPC_TLS_KEY"] = "/certs/" + filepath.Base(server.Key)
	env["INTERNAL_GRPC_TLS_SERVER_NAME"] = "core.test"
	var network string
	for name := range cs[0].NetworkSettings.Networks {
		network = name
		break
	}
	if network == "" {
		t.Fatal("test network missing")
	}
	// t.TempDir's parent remains private; the mounted child must be readable
	// by the image's unprivileged UID. No key is copied into a tracked path.
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{root.Cert, server.Cert, server.Key} {
		if err := os.Chmod(p, 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeEnv := func() string {
		p := filepath.Join(t.TempDir(), "container.env")
		var b strings.Builder
		for k, v := range env {
			b.WriteString(k + "=" + v + "\n")
		}
		if err := os.WriteFile(p, []byte(b.String()), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	start := func() string {
		return strings.TrimSpace(string(command("run", "-d", "--network", network, "--env-file", writeEnv(), "--mount", "type=bind,src="+dir+",dst=/certs,readonly", "-p", "127.0.0.1::51053", cs[0].Image)))
	}
	remove := func(id string) {
		cleanup, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		if err := exec.CommandContext(cleanup, "docker", "rm", "-f", id).Run(); err != nil {
			t.Error("owned TLS container cleanup failed")
		}
	}
	id := start()
	t.Cleanup(func() { remove(id) })
	var state []struct {
		NetworkSettings struct {
			Ports map[string][]struct{ HostIP, HostPort string }
		}
	}
	if json.Unmarshal(command("inspect", id), &state) != nil {
		t.Fatal("cannot inspect TLS probe")
	}
	ports := state[0].NetworkSettings.Ports["51053/tcp"]
	if len(ports) != 1 {
		t.Fatal("TLS probe port missing")
	}
	endpoint := "127.0.0.1:" + ports[0].HostPort
	connect := func(cfg TLS, plain bool) *grpc.ClientConn {
		t.Helper()
		creds, err := ClientTransport(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if plain {
			creds = insecure.NewCredentials()
		}
		c, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(creds))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	good := connect(client, false)
	ready := false
	startupDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(startupDeadline) {
		probe, c := context.WithTimeout(ctx, 500*time.Millisecond)
		_, err := healthpb.NewHealthClient(good).Check(probe, &healthpb.HealthCheckRequest{})
		c()
		if err == nil {
			ready = true
			break
		}
		if ctx.Err() != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatal("deployed TLS core never became available")
	}
	t.Run("business-RPC-requires-grant", func(t *testing.T) {
		probe, c := context.WithTimeout(ctx, time.Second)
		defer c()
		probe = metadata.AppendToOutgoingContext(probe, "authorization", "Bearer "+env["CORE_INTERNAL_SERVICE_TOKEN"])
		_, err := corepb.NewCoreReadServiceClient(good).OpenRetrievalScope(probe, &corepb.OpenScopeRequest{})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatal("deployed TLS core bypassed grant verification")
		}
	})
	for _, tc := range []struct {
		name  string
		cfg   TLS
		plain bool
	}{
		{"wrong-ca", TLS{CA: wrongCA.Cert, Cert: client.Cert, Key: client.Key, ServerName: "core.test"}, false},
		{"wrong-san", TLS{CA: root.Cert, Cert: client.Cert, Key: client.Key, ServerName: "wrong.test"}, false},
		{"expired-client", expired, false}, {"no-client", TLS{CA: root.Cert, ServerName: "core.test"}, false}, {"plaintext", TLS{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := connect(tc.cfg, tc.plain)
			probe, c := context.WithTimeout(ctx, 500*time.Millisecond)
			defer c()
			if _, err := healthpb.NewHealthClient(conn).Check(probe, &healthpb.HealthCheckRequest{}); err == nil {
				t.Fatal("invalid TLS peer accepted by deployed core")
			}
		})
	}
	// A certificate/key mismatch must terminate the actual binary at startup.
	env["INTERNAL_GRPC_TLS_KEY"] = "/certs/" + filepath.Base(root.Key)
	if err := os.Chmod(root.Key, 0644); err != nil {
		t.Fatal(err)
	}
	bad := start()
	defer remove(bad)
	for {
		var rows []struct {
			State struct {
				Running  bool
				ExitCode int
			}
		}
		if json.Unmarshal(command("inspect", bad), &rows) != nil {
			t.Fatal("bad TLS probe inspection failed")
		}
		if !rows[0].State.Running {
			if rows[0].State.ExitCode == 0 {
				t.Fatal("bad TLS process exited successfully")
			}
			break
		}
		if ctx.Err() != nil {
			t.Fatal("bad TLS process did not reject startup")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
