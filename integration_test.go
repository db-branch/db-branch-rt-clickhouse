//go:build integration

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
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

func TestRealClickHouse_ダンプとロードで複数型やビューを含めて往復でき外部エンジンは拒否される(t *testing.T) {
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

	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	type chNode struct {
		inst *pb.InstanceSpec
		cmd  *exec.Cmd
	}
	startNode := func(id string) *chNode {
		t.Helper()
		l, err := net.Listen("tcp", "127.0.0.1:0")
		must("listen "+id, err)
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()

		inst := &pb.InstanceSpec{
			Id: id, RuntimeDir: runtimeDir,
			DataDir: filepath.Join(dir, id, "data"), StateDir: filepath.Join(dir, id, "state"), RunDir: filepath.Join(dir, id, "run"),
			Bind: "127.0.0.1", Ports: map[string]int32{"http": int32(port)},
			Limits: &pb.ResourceLimits{MemoryMb: 1024},
		}
		for _, d := range []string{inst.DataDir, inst.StateDir, inst.RunDir} {
			must("mkdir "+d, os.MkdirAll(d, 0o750))
		}
		req := &pb.InstanceRequest{Instance: inst}
		_, err = rt.Initialize(ctx, req)
		must("Initialize "+id, err)
		_, err = rt.CreateUser(ctx, &pb.CreateUserRequest{Instance: inst, User: "app", Password: "Pw0123456789"})
		must("CreateUser "+id, err)
		plan, err := rt.PrepareStart(ctx, req)
		must("PrepareStart "+id, err)
		cmd := exec.Command(plan.Command, plan.Args...)
		cmd.Dir = "/"
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		must("start "+id, cmd.Start())
		t.Cleanup(func() {
			if cmd.Process != nil && cmd.ProcessState == nil {
				_ = cmd.Process.Signal(syscall.SIGTERM)
				done := make(chan struct{})
				go func() {
					_ = cmd.Wait()
					close(done)
				}()
				select {
				case <-done:
				case <-time.After(15 * time.Second):
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					<-done
				}
			}
		})
		for i := 0; i < 240; i++ {
			if h, _ := rt.Health(ctx, req); h.State == pb.HealthState_HEALTH_STATE_READY {
				return &chNode{inst: inst, cmd: cmd}
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("%s did not become ready", id)
		return nil
	}

	src := startNode("src")
	dst := startNode("dst")

	// Populate source with multiple data types, 3,000 rows, Memory table, View, and MaterializedView.
	seedSQL := strings.Join([]string{
		"CREATE DATABASE shop;",
		"CREATE TABLE shop.orders (" +
			"id UInt64, " +
			"amount Float64, " +
			"status String, " +
			"note Nullable(String), " +
			"created Date, " +
			"tags Array(UInt32)" +
			") ENGINE = MergeTree ORDER BY id;",
		"CREATE MATERIALIZED VIEW shop.mv_status (" +
			"status String, " +
			"cnt UInt64" +
			") ENGINE = SummingMergeTree ORDER BY status " +
			"AS SELECT status, toUInt64(1) AS cnt FROM shop.orders;",
		"INSERT INTO shop.orders " +
			"SELECT number, number * 1.5, concat('st-', toString(number % 5)), " +
			"if(number % 3 = 0, NULL, concat('n-', toString(number))), " +
			"toDate('2026-01-01') + toInt32(number % 365), " +
			"[toUInt32(number), toUInt32(number + 1)] " +
			"FROM numbers(3000);",
		"CREATE TABLE shop.cache (k String, v Int32) ENGINE = Memory;",
		"INSERT INTO shop.cache VALUES ('a', 1), ('b', -2), ('c', 300);",
		"CREATE VIEW shop.v_high AS SELECT count() AS c, sum(id) AS s FROM shop.orders WHERE id >= 1000;",
	}, "\n")
	_, err = rt.ExecAs(ctx, &pb.ExecAsRequest{Instance: src.inst, User: "app", Password: "Pw0123456789", Sql: seedSQL})
	must("seed source", err)

	// Dump from source and verify 0600 permissions.
	dumpPath := filepath.Join(dir, "shop.chdump")
	_, err = rt.DumpSource(ctx, &pb.DumpSourceRequest{
		Source: &pb.RefreshSource{
			Host: "127.0.0.1", Port: src.inst.Ports["http"],
			User: "app", Password: "Pw0123456789",
			Databases: []string{"shop"},
		},
		OutPath: dumpPath,
	})
	must("DumpSource", err)
	fi, err := os.Stat(dumpPath)
	must("stat dump", err)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("dump mode = %04o, want 0600", fi.Mode().Perm())
	}

	// Load into target instance.
	_, err = rt.Load(ctx, &pb.LoadRequest{Instance: dst.inst, DumpPath: dumpPath})
	must("Load", err)

	// Compare row counts and contents across source and target.
	queryRows := func(inst *pb.InstanceSpec, sql string) [][]string {
		t.Helper()
		res, err := rt.Query(ctx, &pb.QueryRequest{Instance: inst, User: "app", Password: "Pw0123456789", Sql: sql, MaxRows: 100})
		must("query "+inst.Id+": "+sql, err)
		out := make([][]string, len(res.Rows))
		for i, r := range res.Rows {
			row := make([]string, len(r.Cells))
			for j, c := range r.Cells {
				if c.IsNull {
					row[j] = "<NULL>"
				} else {
					row[j] = c.Value
				}
			}
			out[i] = row
		}
		return out
	}

	checks := []string{
		"SELECT count(), sum(id), round(sum(amount), 2), countIf(isNull(note)) FROM shop.orders",
		"SELECT id, amount, status, note, created, tags FROM shop.orders WHERE id IN (0, 1, 1234, 2999) ORDER BY id",
		"SELECT k, v FROM shop.cache ORDER BY k",
		"SELECT c, s FROM shop.v_high",
		"SELECT status, sum(cnt) FROM shop.mv_status GROUP BY status ORDER BY status",
	}
	for _, sql := range checks {
		want := queryRows(src.inst, sql)
		got := queryRows(dst.inst, sql)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("mismatch for %s:\nsrc = %v\ndst = %v", sql, want, got)
		}
	}

	// External-engine table on source must abort DumpSource with table & engine in the error and leave no file behind.
	adminReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/", src.inst.Ports["http"]),
		strings.NewReader("CREATE TABLE shop.ext_url (line String) ENGINE = URL('http://127.0.0.1:1/', 'LineAsString')"))
	must("new admin request", err)
	adminReq.SetBasicAuth("admin", "")
	adminResp, err := http.DefaultClient.Do(adminReq)
	must("create external table", err)
	adminBody, _ := io.ReadAll(adminResp.Body)
	_ = adminResp.Body.Close()
	if adminResp.StatusCode != http.StatusOK {
		t.Fatalf("create external table status %d: %s", adminResp.StatusCode, adminBody)
	}

	rejectedPath := filepath.Join(dir, "rejected.chdump")
	_, err = rt.DumpSource(ctx, &pb.DumpSourceRequest{
		Source: &pb.RefreshSource{
			Host: "127.0.0.1", Port: src.inst.Ports["http"],
			User: "app", Password: "Pw0123456789",
			Databases: []string{"shop"},
		},
		OutPath: rejectedPath,
	})
	if err == nil {
		t.Fatal("DumpSource must reject a database containing an external-engine table")
	}
	if !strings.Contains(err.Error(), "shop.ext_url") || !strings.Contains(err.Error(), "URL") {
		t.Fatalf("rejection error must name table and engine, got: %v", err)
	}
	if _, statErr := os.Stat(rejectedPath); statErr == nil {
		t.Fatal("partial dump file must not remain after rejection")
	}
}
