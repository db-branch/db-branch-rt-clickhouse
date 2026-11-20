//go:build integration

package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	pb "github.com/db-branch/db-branch/pkg/ext/proto/runtime/v1"
	"github.com/db-branch/db-branch/pkg/ext/runtimeplugin"
)

// Runs the module, as the core does (go-plugin, gRPC), against a real
// ClickHouse started the way the module's LaunchPlan says. It needs no root
// and no systemd (db-branch-conformance --lifecycle covers those).
//
//	DBB_CH_MODULE=/path/to/db-branch-rt-clickhouse DBB_CH_BINARY=/path/to/clickhouse \
//	    go test -tags integration -run TestRealClickHouse ./...
func TestRealClickHouse_モジュールの契約どおりに起動し操作できる(t *testing.T) {
	modBin, chBin := os.Getenv("DBB_CH_MODULE"), os.Getenv("DBB_CH_BINARY")
	if modBin == "" || chBin == "" {
		t.Skip("DBB_CH_MODULE and DBB_CH_BINARY are needed")
	}
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: runtimeplugin.Handshake, Plugins: goplugin.PluginSet{runtimeplugin.Name: &runtimeplugin.Plugin{}},
		Cmd: exec.Command(modBin), AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC}, SkipHostEnv: false,
	})
	defer client.Kill()
	rpc, err := client.Client()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := rpc.Dispense(runtimeplugin.Name)
	if err != nil {
		t.Fatal(err)
	}
	rt := pb.NewRuntimeClient(raw.(*grpc.ClientConn))
	ctx := context.Background()

	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(chBin, filepath.Join(runtimeDir, "clickhouse")); err != nil {
		t.Fatal(err)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	inst := &pb.InstanceSpec{
		Id: "it", RuntimeDir: runtimeDir, DataDir: filepath.Join(dir, "data"), StateDir: filepath.Join(dir, "state"),
		RunDir: filepath.Join(dir, "run"), Bind: "127.0.0.1", Ports: map[string]int32{"http": int32(port)},
		Limits: &pb.ResourceLimits{MemoryMb: 1024},
	}
	for _, d := range []string{inst.DataDir, inst.StateDir, inst.RunDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	req := &pb.InstanceRequest{Instance: inst}
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	_, err = rt.Initialize(ctx, req)
	must("Initialize", err)
	_, err = rt.CreateUser(ctx, &pb.CreateUserRequest{Instance: inst, User: "app", Password: "Pw0123456789"})
	must("CreateUser", err)

	var server *exec.Cmd
	start := func() {
		t.Helper()
		plan, err := rt.PrepareStart(ctx, req)
		must("PrepareStart", err)
		server = exec.Command(plan.Command, plan.Args...)
		server.Dir = "/"
		server.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // watchdog and server share a group, as in a unit's cgroup
		must("start clickhouse", server.Start())
		for i := 0; i < 240; i++ {
			if h, _ := rt.Health(ctx, req); h.State == pb.HealthState_HEALTH_STATE_READY {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatal("clickhouse did not become ready")
	}
	stop := func(sig syscall.Signal) {
		t.Helper()
		if sig == syscall.SIGKILL {
			_ = syscall.Kill(-server.Process.Pid, syscall.SIGKILL) // the whole group, as systemd does at the end of a stop
		} else {
			_ = server.Process.Signal(sig) // the main process only (KillMode=mixed)
		}
		_ = server.Wait()
	}
	defer func() {
		if server != nil && server.ProcessState == nil {
			_ = server.Process.Kill()
			_ = server.Wait()
		}
	}()
	q := func(user, pw, sql string) (*pb.QueryResponse, error) {
		return rt.Query(ctx, &pb.QueryRequest{Instance: inst, User: user, Password: pw, Sql: sql, MaxRows: 100})
	}

	start()
	_, err = rt.ExecAs(ctx, &pb.ExecAsRequest{Instance: inst, User: "app", Password: "Pw0123456789",
		Sql: "CREATE DATABASE d; CREATE TABLE d.t (x UInt32) ENGINE = MergeTree ORDER BY x; INSERT INTO d.t VALUES (1),(2),(3);"})
	must("ExecAs", err)
	count := func() string {
		t.Helper()
		r, err := q("app", "Pw0123456789", "SELECT count() FROM d.t")
		must("count", err)
		return r.Rows[0].Cells[0].Value
	}
	if got := count(); got != "3" {
		t.Fatalf("count = %s", got)
	}
	// The login is not an admin and cannot reach the node.
	for _, sql := range []string{"SELECT * FROM file('/etc/passwd', 'LineAsString')", "SELECT * FROM url('http://127.0.0.1:1/', 'LineAsString')", "SYSTEM RELOAD CONFIG", "CREATE USER x IDENTIFIED BY 'y'"} {
		if _, err := q("app", "Pw0123456789", sql); err == nil {
			t.Errorf("the login must not be able to run: %s", sql)
		}
	}
	if _, err := q("app", "wrong", "SELECT 1"); err == nil {
		t.Error("a wrong password must be refused")
	}

	stop(syscall.SIGTERM)
	_, err = rt.VerifyCleanShutdown(ctx, req)
	must("VerifyCleanShutdown after SIGTERM", err)
	start()
	if got := count(); got != "3" {
		t.Fatalf("data lost over a restart: %s", got)
	}
	stop(syscall.SIGKILL)
	if _, err := rt.VerifyCleanShutdown(ctx, req); err == nil {
		t.Error("a killed server must not count as a clean shutdown")
	}
	_, err = rt.PrepareClone(ctx, req)
	must("PrepareClone", err)
	start()
	if got := count(); got != "3" {
		t.Fatalf("data lost over a kill: %s", got)
	}
	stop(syscall.SIGTERM)
}
