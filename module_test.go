package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/db-branch/db-branch/pkg/ext/proto/runtime/v1"
)

// instanceFor points an instance at the test server's port and makes its directories.
func instanceFor(t *testing.T, srv *httptest.Server) *pb.InstanceSpec {
	t.Helper()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	p, _ := strconv.Atoi(port)
	dir := t.TempDir()
	i := &pb.InstanceSpec{
		Id: "branch-x", RuntimeDir: "/rt", DataDir: filepath.Join(dir, "data"), StateDir: filepath.Join(dir, "state"),
		Bind: "127.0.0.1", Ports: map[string]int32{"http": int32(p)},
	}
	for _, d := range []string{i.DataDir, i.StateDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	return i
}

func TestDescribe_ClickHouseの宣言(t *testing.T) {
	d, _ := newModule().Describe(context.Background(), nil)
	if d.Engine != "clickhouse" || d.RunAsUser != "db-branch-rt-clickhouse" || len(d.Protocols) != 1 || !d.Protocols[0].Primary ||
		!d.Capabilities.Query || !d.Capabilities.Checkpoints || d.Capabilities.Upgrade || d.ZfsProps["recordsize"] != "128K" {
		t.Fatalf("%+v", d)
	}
}

func TestPrepareStart_設定とユーザーを書いて起動計画を返す(t *testing.T) {
	m := newModule()
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	i := instanceFor(t, srv)
	i.RuntimeDir = "/var/lib/db-branch/runtimes/clickhouse/24.8"
	plan, err := m.PrepareStart(context.Background(), &pb.InstanceRequest{Instance: i})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Command != i.RuntimeDir+"/clickhouse" || plan.Args[0] != "server" || plan.Args[1] != "--config-file="+i.StateDir+"/config.xml" ||
		plan.KillSignal != "SIGTERM" || plan.KillMode != "mixed" || plan.TimeoutStopSec != 120 {
		t.Fatalf("%+v", plan)
	}
	for _, f := range []string{"config.xml", "users.xml"} {
		if _, err := os.Stat(filepath.Join(i.StateDir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	for _, d := range []string{"tmp", "user_files", "format_schemas", "access"} {
		if _, err := os.Stat(filepath.Join(i.DataDir, d)); err != nil {
			t.Errorf("data dir %s: %v", d, err)
		}
	}
	i.Ports = nil
	if _, err := m.PrepareStart(context.Background(), &pb.InstanceRequest{Instance: i}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no port: %v", err)
	}
}

func TestCreateUser_繰り返しても同じでパスワードは上書きされる(t *testing.T) {
	m := newModule()
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	i := instanceFor(t, srv)
	for _, pw := range []string{"one", "one", "two"} {
		if _, err := m.CreateUser(context.Background(), &pb.CreateUserRequest{Instance: i, User: "app", Password: pw}); err != nil {
			t.Fatal(err)
		}
	}
	x, _ := os.ReadFile(filepath.Join(i.StateDir, "users.xml"))
	if strings.Count(string(x), "<app>") != 1 || !strings.Contains(string(x), hashPassword("two")) || strings.Contains(string(x), hashPassword("one")) {
		t.Fatalf("users.xml:\n%s", x)
	}
	// The password is never stored in the clear.
	j, _ := os.ReadFile(filepath.Join(i.StateDir, "logins.json"))
	if strings.Contains(string(j)+string(x), "two\"") {
		t.Fatalf("clear password stored: %s", j)
	}
}

func TestHealth_pingの応答で状態を返す(t *testing.T) {
	m := newModule()
	code := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ping" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.WriteHeader(code)
	}))
	i := instanceFor(t, srv)
	h, _ := m.Health(context.Background(), &pb.InstanceRequest{Instance: i})
	if h.State != pb.HealthState_HEALTH_STATE_READY {
		t.Fatalf("ready: %v", h.State)
	}
	code = http.StatusServiceUnavailable
	if h, _ = m.Health(context.Background(), &pb.InstanceRequest{Instance: i}); h.State != pb.HealthState_HEALTH_STATE_STARTING {
		t.Fatalf("starting: %v", h.State)
	}
	srv.Close()
	if h, _ = m.Health(context.Background(), &pb.InstanceRequest{Instance: i}); h.State != pb.HealthState_HEALTH_STATE_STOPPED {
		t.Fatalf("stopped: %v", h.State)
	}
}

func TestQuery_ログインで実行し結果を返す(t *testing.T) {
	m := newModule()
	var gotUser, gotPass, gotSQL, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		b, _ := io.ReadAll(r.Body)
		gotSQL, gotQuery = string(b), r.URL.RawQuery
		io.WriteString(w, `{"meta":[{"name":"n","type":"UInt8"}],"data":[[1],[2],[3]],"rows":3}`)
	}))
	defer srv.Close()
	i := instanceFor(t, srv)
	res, err := m.Query(context.Background(), &pb.QueryRequest{Instance: i, User: "app", Password: "pw", Sql: "SELECT n", MaxRows: 2})
	if err != nil {
		t.Fatal(err)
	}
	if gotUser != "app" || gotPass != "pw" || gotSQL != "SELECT n" || !strings.Contains(gotQuery, "max_result_rows=3") || !strings.Contains(gotQuery, "result_overflow_mode=break") {
		t.Fatalf("user %q pass %q sql %q query %q", gotUser, gotPass, gotSQL, gotQuery)
	}
	if len(res.Rows) != 2 || !res.Truncated {
		t.Fatalf("%+v", res)
	}
}

func TestQuery_結果のない文は件数なしで成功する_失敗はメッセージを返す(t *testing.T) {
	m := newModule()
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, "Code: 62. DB::Exception: Syntax error\n")
		}
	}))
	defer srv.Close()
	i := instanceFor(t, srv)
	res, err := m.Query(context.Background(), &pb.QueryRequest{Instance: i, User: "app", Sql: "CREATE TABLE t (x UInt8) ENGINE = Memory"})
	if err != nil || !res.HasAffected {
		t.Fatalf("%+v %v", res, err)
	}
	fail = true
	_, err = m.Query(context.Background(), &pb.QueryRequest{Instance: i, User: "app", Sql: "bad"})
	if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "Syntax error") {
		t.Fatalf("err = %v", err)
	}
	srv.Close()
	if _, err = m.Query(context.Background(), &pb.QueryRequest{Instance: i, Sql: "SELECT 1"}); status.Code(err) != codes.Unavailable {
		t.Fatalf("server down: %v", err)
	}
}

func TestExecAs_文を一つずつログインとして実行する(t *testing.T) {
	m := newModule()
	var stmts, users []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _, _ := r.BasicAuth()
		users = append(users, u)
		b, _ := io.ReadAll(r.Body)
		stmts = append(stmts, string(b))
	}))
	defer srv.Close()
	i := instanceFor(t, srv)
	if _, err := m.ExecAs(context.Background(), &pb.ExecAsRequest{Instance: i, User: "app", Password: "pw", Sql: "CREATE DATABASE d; CREATE TABLE d.t (x UInt8) ENGINE = Memory;"}); err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 2 || stmts[0] != "CREATE DATABASE d" || users[0] != "app" || users[1] != "app" {
		t.Fatalf("%q %q", stmts, users)
	}
}

func TestVerifyCleanShutdownとPrepareClone_statusファイルで判断する(t *testing.T) {
	m := newModule()
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	i := instanceFor(t, srv)
	r := &pb.InstanceRequest{Instance: i}
	if _, err := m.VerifyCleanShutdown(context.Background(), r); err != nil {
		t.Fatalf("no status file is a clean shutdown: %v", err)
	}
	_ = os.WriteFile(filepath.Join(i.DataDir, "status"), []byte("PID: 1\n"), 0o640)
	if _, err := m.VerifyCleanShutdown(context.Background(), r); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a leftover status file is not clean: %v", err)
	}
	if _, err := m.PrepareClone(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(i.DataDir, "status")); err == nil {
		t.Fatal("PrepareClone must remove the status file")
	}
}

func TestAdminAccessとFormatDSNとTailLogとRemove(t *testing.T) {
	m := newModule()
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	i := instanceFor(t, srv)
	a, _ := m.AdminAccess(context.Background(), &pb.InstanceRequest{Instance: i})
	if a.User != "admin" || a.Host != "127.0.0.1" || a.Port != i.Ports["http"] || a.Endpoints["http"].Port != i.Ports["http"] {
		t.Fatalf("%+v", a)
	}
	d, _ := m.FormatDSN(context.Background(), &pb.FormatDSNRequest{Instance: i, Host: "node1", User: "app", Password: "p w"})
	if !strings.HasPrefix(d.Dsn, "clickhouse://app:p%20w@node1:") || !strings.HasSuffix(d.Dsn, "/") {
		t.Fatalf("dsn = %s", d.Dsn)
	}
	_ = os.WriteFile(filepath.Join(i.StateDir, "clickhouse-server.log"), []byte("a\nb\nc\n"), 0o640)
	if l, _ := m.TailLog(context.Background(), &pb.TailLogRequest{Instance: i, Lines: 2}); l.Text != "b\nc\n" {
		t.Fatalf("tail = %q", l.Text)
	}
	if _, err := m.TailLog(context.Background(), &pb.TailLogRequest{Instance: &pb.InstanceSpec{StateDir: "/nonexistent"}}); err == nil {
		t.Fatal("missing log must be an error")
	}
	if _, err := m.RemoveInstance(context.Background(), &pb.InstanceRequest{Instance: i}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(i.StateDir, "clickhouse-server.log")); err == nil {
		t.Fatal("log must be removed")
	}
}
