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
	"sync"
	"testing"
	"time"

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

func cloneInstance(i *pb.InstanceSpec) *pb.InstanceSpec {
	ports := make(map[string]int32, len(i.Ports))
	for k, v := range i.Ports {
		ports[k] = v
	}
	return &pb.InstanceSpec{
		Id: i.Id, Engine: i.Engine, Version: i.Version, RuntimeDir: i.RuntimeDir,
		DataDir: i.DataDir, StateDir: i.StateDir, RunDir: i.RunDir, Bind: i.Bind,
		Ports: ports, Maintenance: i.Maintenance,
	}
}

func TestDescribe_ClickHouseの宣言(t *testing.T) {
	d, _ := newModule().Describe(context.Background(), nil)
	if d.Engine != "clickhouse" || d.RunAsUser != "db-branch-rt-clickhouse" || len(d.Protocols) != 1 || !d.Protocols[0].Primary ||
		!d.Capabilities.Query || !d.Capabilities.Checkpoints || !d.Capabilities.LiveFreeze || !d.Capabilities.Upgrade ||
		d.Capabilities.SourceDump || d.Capabilities.BranchMerge || d.Capabilities.HistoryRecord ||
		d.Capabilities.UpdateStats || d.Capabilities.InstanceMetrics || d.ZfsProps["recordsize"] != "128K" {
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

func TestCatalog_固定したLTSのtgzをURLとダイジェストつきで返す(t *testing.T) {
	r, err := newModule().Catalog(context.Background(), &pb.CatalogRequest{})
	if err != nil || len(r.Entries) == 0 {
		t.Fatalf("%v %v", r, err)
	}
	for _, e := range r.Entries {
		if !strings.HasPrefix(e.Url, "https://packages.clickhouse.com/tgz/lts/clickhouse-common-static-"+e.Version+"-") ||
			len(e.Sha256) != 64 || e.Size <= 0 || (e.Platform != "amd64" && e.Platform != "arm64") ||
			!strings.HasSuffix(e.Url, "-"+e.Platform+".tgz") {
			t.Errorf("entry %+v", e)
		}
	}
}

func TestCatalog_amd64とarm64の両方を同じ版で返す(t *testing.T) {
	r, _ := newModule().Catalog(context.Background(), &pb.CatalogRequest{})
	got := map[string]string{}
	for _, e := range r.Entries {
		got[e.Version+"/"+e.Platform] = e.Sha256
	}
	want := map[string]string{
		"26.3.19.3/amd64": "8c9886db49555efb6bef2ee08656af0d48bd07c83a08a729abb9d60e26bfff3d",
		"26.3.19.3/arm64": "f7df37b5c600099bc6e772c9bae2b91a530c6bbc07a2bd5a5021443c742c4c92",
	}
	for k, sum := range want {
		if got[k] != sum {
			t.Errorf("%s: sha256 %q, want %q", k, got[k], sum)
		}
	}
}

func TestPrepareStart_展開したtgzのusr_bin_clickhouseを優先する(t *testing.T) {
	m := newModule()
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	i := instanceFor(t, srv)
	i.RuntimeDir = filepath.Join(t.TempDir(), "rt")
	plain := filepath.Join(i.RuntimeDir, "clickhouse")
	packaged := filepath.Join(i.RuntimeDir, "usr", "bin", "clickhouse")
	if err := os.MkdirAll(filepath.Dir(packaged), 0o755); err != nil {
		t.Fatal(err)
	}
	// Neither exists yet: the plain name (a hand-placed binary) is the default.
	plan, _ := m.PrepareStart(context.Background(), &pb.InstanceRequest{Instance: i})
	if plan.Command != plain {
		t.Fatalf("default = %s", plan.Command)
	}
	_ = os.WriteFile(packaged, []byte("#!/bin/sh\n"), 0o755)
	plan, _ = m.PrepareStart(context.Background(), &pb.InstanceRequest{Instance: i})
	if plan.Command != packaged {
		t.Fatalf("packaged = %s", plan.Command)
	}
}

func TestPrepareRuntime_動くバイナリだけを受け入れる(t *testing.T) {
	m := newModule()
	dir := t.TempDir()
	bin := filepath.Join(dir, "usr", "bin")
	_ = os.MkdirAll(bin, 0o755)
	if _, err := m.PrepareRuntime(context.Background(), &pb.PrepareRuntimeRequest{Version: "1", StagingDir: dir}); status.Code(err) != codes.Internal {
		t.Fatalf("no binary: %v", err)
	}
	_ = os.WriteFile(filepath.Join(bin, "clickhouse"), []byte("#!/bin/sh\necho ClickHouse\n"), 0o755)
	if _, err := m.PrepareRuntime(context.Background(), &pb.PrepareRuntimeRequest{Version: "1", StagingDir: dir}); err != nil {
		t.Fatalf("a binary that runs: %v", err)
	}
	_ = os.WriteFile(filepath.Join(bin, "clickhouse"), []byte("#!/bin/sh\nexit 3\n"), 0o755)
	if _, err := m.PrepareRuntime(context.Background(), &pb.PrepareRuntimeRequest{Version: "1", StagingDir: dir}); err == nil {
		t.Fatal("a binary that fails must be refused")
	}
}

func TestHTTP_接続を使い回さない_ClickHouseは停止時に残った接続を待つ(t *testing.T) {
	m := newModule()
	var closes []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		closes = append(closes, r.Close) // the client asked for "Connection: close"
		io.WriteString(w, "Ok.")
	}))
	defer srv.Close()
	i := instanceFor(t, srv)
	_, _ = m.Health(context.Background(), &pb.InstanceRequest{Instance: i})
	_, _ = m.Query(context.Background(), &pb.QueryRequest{Instance: i, User: "app", Sql: "SELECT 1"})
	if len(closes) != 2 || !closes[0] || !closes[1] {
		t.Fatalf("keep-alive connections would hold ClickHouse's shutdown for seconds: %v", closes)
	}
}

func TestPreFreeze_管理者資格でフラッシュと停止を行いマージ完了を待ってトークンを返す(t *testing.T) {
	m := newModule()
	m.freezePollInterval = 5 * time.Millisecond
	var mu sync.Mutex
	var stmts []string
	mergesChecks := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "admin" || p != "" {
			t.Errorf("expected admin credentials, got %q/%q (ok=%v)", u, p, ok)
		}
		b, _ := io.ReadAll(r.Body)
		sql := string(b)
		mu.Lock()
		stmts = append(stmts, sql)
		if sql == "SELECT count() FROM system.merges" {
			mergesChecks++
			if mergesChecks == 1 {
				mu.Unlock()
				io.WriteString(w, `{"meta":[{"name":"count()","type":"UInt64"}],"data":[["1"]],"rows":1}`)
				return
			}
			mu.Unlock()
			io.WriteString(w, `{"meta":[{"name":"count()","type":"UInt64"}],"data":[["0"]],"rows":1}`)
			return
		}
		mu.Unlock()
	}))
	defer srv.Close()
	i := instanceFor(t, srv)

	tok, err := m.PreFreeze(context.Background(), &pb.InstanceRequest{Instance: i})
	if err != nil {
		t.Fatalf("PreFreeze: %v", err)
	}
	want := []string{
		"SYSTEM FLUSH ASYNC INSERT QUEUE",
		"SYSTEM STOP MERGES",
		"SYSTEM STOP MOVES",
		"SELECT count() FROM system.merges",
		"SELECT count() FROM system.merges",
	}
	if strings.Join(stmts, "\n") != strings.Join(want, "\n") {
		t.Fatalf("statements =\n%s\nwant =\n%s", strings.Join(stmts, "\n"), strings.Join(want, "\n"))
	}
	st, ok := decodeFreezeToken(tok.GetData())
	if !ok || !st.StoppedMerges || !st.StoppedMoves || st.Host != "127.0.0.1" || st.Port != i.Ports["http"] {
		t.Fatalf("token = %q (%+v, ok=%v)", tok.GetData(), st, ok)
	}

	iNoPort := &pb.InstanceSpec{Id: "x"}
	if _, err := m.PreFreeze(context.Background(), &pb.InstanceRequest{Instance: iNoPort}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no port: %v", err)
	}
}

func TestPreFreeze_マージ完了がタイムアウトしたら停止を戻してからエラーを返す(t *testing.T) {
	m := newModule()
	m.freezeTimeout = 50 * time.Millisecond
	m.freezePollInterval = 10 * time.Millisecond
	var mu sync.Mutex
	var stmts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sql := string(b)
		mu.Lock()
		stmts = append(stmts, sql)
		mu.Unlock()
		if sql == "SELECT count() FROM system.merges" {
			io.WriteString(w, `{"meta":[{"name":"count()","type":"UInt64"}],"data":[["2"]],"rows":1}`)
		}
	}))
	defer srv.Close()
	i := instanceFor(t, srv)

	_, err := m.PreFreeze(context.Background(), &pb.InstanceRequest{Instance: i})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	joined := strings.Join(stmts, "\n")
	if !strings.Contains(joined, "SYSTEM START MOVES") || !strings.Contains(joined, "SYSTEM START MERGES") {
		t.Fatalf("must resume moves and merges on timeout:\n%s", joined)
	}

	// 呼び出し元のコンテキストが先に切れてもロールバックは完了する
	stmts = nil
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	m.freezeTimeout = 5 * time.Second
	if _, err := m.PreFreeze(ctx, &pb.InstanceRequest{Instance: i}); err == nil {
		t.Fatal("expected error when caller ctx expires")
	}
	joined = strings.Join(stmts, "\n")
	if !strings.Contains(joined, "SYSTEM START MOVES") || !strings.Contains(joined, "SYSTEM START MERGES") {
		t.Fatalf("must resume moves and merges even when caller ctx expired:\n%s", joined)
	}

	// タイムアウト時のロールバック自体が失敗した場合もエラーにその旨を含める
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch string(b) {
		case "SELECT count() FROM system.merges":
			io.WriteString(w, `{"meta":[{"name":"count()","type":"UInt64"}],"data":[["1"]],"rows":1}`)
		case "SYSTEM START MOVES", "SYSTEM START MERGES":
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "resume failed")
		}
	}))
	defer srv2.Close()
	m.freezeTimeout = 30 * time.Millisecond
	i2 := instanceFor(t, srv2)
	i2.Bind = "0.0.0.0" // adminHost が 127.0.0.1 に丸めることも確認
	_, err = m.PreFreeze(context.Background(), &pb.InstanceRequest{Instance: i2})
	if status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("expected rollback failure detail, got %v", err)
	}
}

func TestPreFreeze_途中で失敗したら停止済みのマージと移動を元に戻す(t *testing.T) {
	m := newModule()
	failOn := "SYSTEM STOP MOVES"
	badMergesJSON := false
	var stmts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sql := string(b)
		stmts = append(stmts, sql)
		if sql == failOn {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "boom")
			return
		}
		if badMergesJSON && sql == "SELECT count() FROM system.merges" {
			io.WriteString(w, "not json")
		}
	}))
	defer srv.Close()
	i := instanceFor(t, srv)

	for _, stage := range []string{"SYSTEM STOP MERGES", "SYSTEM STOP MOVES", "SELECT count() FROM system.merges"} {
		stmts = nil
		failOn = stage
		if _, err := m.PreFreeze(context.Background(), &pb.InstanceRequest{Instance: i}); err == nil {
			t.Fatalf("expected error on %s failure", stage)
		}
		joined := strings.Join(stmts, "\n")
		if !strings.Contains(joined, "SYSTEM START MERGES") || !strings.Contains(joined, "SYSTEM START MOVES") {
			t.Fatalf("must resume merges and moves after %s failed:\n%s", stage, joined)
		}
	}

	// system.merges の応答が壊れていてもロールバックしてエラーを返す
	stmts = nil
	failOn = ""
	badMergesJSON = true
	if _, err := m.PreFreeze(context.Background(), &pb.InstanceRequest{Instance: i}); status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal on corrupt merges response, got %v", err)
	}
	if !strings.Contains(strings.Join(stmts, "\n"), "SYSTEM START MERGES") {
		t.Fatalf("must resume merges on corrupt response: %q", stmts)
	}

	// 最初のフラッシュで失敗した場合は STOP せず即座に失敗する
	stmts = nil
	badMergesJSON = false
	failOn = "SYSTEM FLUSH ASYNC INSERT QUEUE"
	if _, err := m.PreFreeze(context.Background(), &pb.InstanceRequest{Instance: i}); err == nil {
		t.Fatal("expected error on FLUSH failure")
	}
	if len(stmts) != 1 {
		t.Fatalf("unexpected statements after FLUSH failure: %q", stmts)
	}
}

func TestPostFreeze_トークン有無によらずマージと移動を再開し冪等に成功する(t *testing.T) {
	m := newModule()
	var stmts []string
	failMoves := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "admin" || p != "" {
			t.Errorf("expected admin credentials, got %q/%q (ok=%v)", u, p, ok)
		}
		b, _ := io.ReadAll(r.Body)
		sql := string(b)
		stmts = append(stmts, sql)
		if failMoves && sql == "SYSTEM START MOVES" {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "moves failed")
		}
	}))
	defer srv.Close()
	i := instanceFor(t, srv)
	tok := &pb.FreezeToken{Data: encodeFreezeToken(freezeState{Host: "127.0.0.1", Port: i.Ports["http"], StoppedMerges: true, StoppedMoves: true})}

	// 2 回呼んでも冪等に成功する
	for n := 0; n < 2; n++ {
		if _, err := m.PostFreeze(context.Background(), &pb.PostFreezeRequest{Instance: i, Token: tok}); err != nil {
			t.Fatalf("PostFreeze #%d: %v", n+1, err)
		}
	}
	want := "SYSTEM START MOVES\nSYSTEM START MERGES\nSYSTEM START MOVES\nSYSTEM START MERGES"
	if strings.Join(stmts, "\n") != want {
		t.Fatalf("stmts =\n%s\nwant =\n%s", strings.Join(stmts, "\n"), want)
	}

	// クラッシュ復旧時のように空トークンでも両方を再開する
	stmts = nil
	if _, err := m.PostFreeze(context.Background(), &pb.PostFreezeRequest{Instance: i}); err != nil {
		t.Fatalf("PostFreeze empty token: %v", err)
	}
	if strings.Join(stmts, "\n") != "SYSTEM START MOVES\nSYSTEM START MERGES" {
		t.Fatalf("empty token stmts = %q", stmts)
	}

	// Instance にポートが無くてもトークンの接続先で再開できる
	stmts = nil
	if _, err := m.PostFreeze(context.Background(), &pb.PostFreezeRequest{Instance: &pb.InstanceSpec{Id: "x"}, Token: tok}); err != nil {
		t.Fatalf("PostFreeze token fallback: %v", err)
	}
	if len(stmts) != 2 {
		t.Fatalf("token fallback stmts = %q", stmts)
	}

	// START MOVES が失敗しても START MERGES は必ず試み、エラーを返す
	stmts = nil
	failMoves = true
	if _, err := m.PostFreeze(context.Background(), &pb.PostFreezeRequest{Instance: i, Token: tok}); err == nil {
		t.Fatal("expected error when START MOVES fails")
	}
	if strings.Join(stmts, "\n") != "SYSTEM START MOVES\nSYSTEM START MERGES" {
		t.Fatalf("must still attempt START MERGES: %q", stmts)
	}

	// ポートもトークンも無い場合は InvalidArgument
	if _, err := m.PostFreeze(context.Background(), &pb.PostFreezeRequest{Instance: &pb.InstanceSpec{Id: "x"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no port and no token: %v", err)
	}
}

func TestCheckUpgradePath_新しい版だけ許可し同じ版や古い版や不正な版を拒否する(t *testing.T) {
	m := newModule()
	ctx := context.Background()
	for _, okCase := range [][2]string{
		{"25.8.1.1", "26.3.19.3"},
		{"24.8.1.1", "26.3.19.3"}, // LTS スキップも拒否しない
		{"26.3.19.3", "26.4.1.1"}, // 非 LTS も拒否しない
		{"1.0", "2.0"},            // conformance プローブ
	} {
		if _, err := m.CheckUpgradePath(ctx, &pb.CheckUpgradePathRequest{FromVersion: okCase[0], ToVersion: okCase[1]}); err != nil {
			t.Errorf("%s -> %s: unexpected error: %v", okCase[0], okCase[1], err)
		}
	}
	for _, sameOrDown := range [][2]string{
		{"26.3.19.3", "26.3.19.3"},
		{"26.3.19.3", "26.3.2.1"},
		{"26.3.19.3", "25.8.1.1"},
	} {
		_, err := m.CheckUpgradePath(ctx, &pb.CheckUpgradePathRequest{FromVersion: sameOrDown[0], ToVersion: sameOrDown[1]})
		if status.Code(err) != codes.FailedPrecondition {
			t.Errorf("%s -> %s: got %v, want FailedPrecondition", sameOrDown[0], sameOrDown[1], err)
		}
	}
	if _, err := m.CheckUpgradePath(ctx, &pb.CheckUpgradePathRequest{FromVersion: "bad", ToVersion: "26.3.19.3"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid from: %v", err)
	}
	if _, err := m.CheckUpgradePath(ctx, &pb.CheckUpgradePathRequest{FromVersion: "26.3.19.3", ToVersion: "bad"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid to: %v", err)
	}
}

func TestCheckUpgrade_導入済みランタイムとデータディレクトリと稼働中サーバーを副作用なく確認する(t *testing.T) {
	m := newModule()
	var seenSQL []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "admin" || p != "" {
			t.Errorf("CheckUpgrade must query as admin, got %q/%q (ok=%v)", u, p, ok)
		}
		b, _ := io.ReadAll(r.Body)
		seenSQL = append(seenSQL, string(b))
		io.WriteString(w, `{"meta":[{"name":"tables","type":"UInt64"},{"name":"parts","type":"UInt64"}],"data":[["3","12"]],"rows":1}`)
	}))
	defer srv.Close()

	rtRoot := t.TempDir()
	installFakeCH := func(ver string) string {
		dir := filepath.Join(rtRoot, ver)
		bin := filepath.Join(dir, "usr", "bin", "clickhouse")
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bin, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	fromDir := installFakeCH("25.8.1.1")
	installFakeCH("26.3.19.3")
	installFakeCH("26.4.1.1")

	i := instanceFor(t, srv)
	i.Version = "25.8.1.1"
	i.RuntimeDir = fromDir

	plan, err := m.CheckUpgrade(context.Background(), &pb.CheckUpgradeRequest{
		Instance: i, ToVersion: "26.3.19.3", Login: &pb.Login{User: "app", Password: "pw"},
	})
	if err != nil {
		t.Fatalf("CheckUpgrade: %v", err)
	}
	if plan.LegacyAuth {
		t.Fatal("ClickHouse does not use LegacyAuth")
	}
	if len(seenSQL) != 1 || !strings.Contains(seenSQL[0], "system.tables") || !strings.Contains(seenSQL[0], "system.parts") {
		t.Fatalf("expected lightweight system.tables/system.parts query, got %q", seenSQL)
	}
	hasInfo := false
	for _, f := range plan.Findings {
		if f.Level == "error" || f.Level == "warning" {
			t.Errorf("adjacent LTS should have no error/warning, got %+v", f)
		}
		if f.Level == "info" && strings.Contains(f.Message, "3") && strings.Contains(f.Message, "12") {
			hasInfo = true
		}
	}
	if !hasInfo {
		t.Fatalf("expected info finding with table/part counts, got %+v", plan.Findings)
	}
	// 副作用なし: StateDir も DataDir も変更されていない
	for _, dir := range []string{i.StateDir, i.DataDir} {
		entries, _ := os.ReadDir(dir)
		if len(entries) != 0 {
			t.Fatalf("CheckUpgrade must have no side effects in %s, found %v", dir, entries)
		}
	}

	// LTS 飛ばし (24.8.1.1 -> 26.3.19.3) はエラーではなく警告を返す
	iSkip := cloneInstance(i)
	iSkip.Version = "24.8.1.1"
	plan, err = m.CheckUpgrade(context.Background(), &pb.CheckUpgradeRequest{Instance: iSkip, ToVersion: "26.3.19.3"})
	if err != nil {
		t.Fatalf("LTS skip CheckUpgrade: %v", err)
	}
	hasWarn := false
	for _, f := range plan.Findings {
		if f.Level == "error" {
			t.Errorf("LTS skip must not be an error: %+v", f)
		}
		if f.Level == "warning" && strings.Contains(f.Message, "25.3") && strings.Contains(f.Message, "25.8") {
			hasWarn = true
		}
	}
	if !hasWarn {
		t.Fatalf("LTS skip must emit warning: %+v", plan.Findings)
	}

	// 非 LTS への移行も警告を返す
	iCur := cloneInstance(i)
	iCur.Version = "26.3.19.3"
	plan, err = m.CheckUpgrade(context.Background(), &pb.CheckUpgradeRequest{Instance: iCur, ToVersion: "26.4.1.1"})
	if err != nil {
		t.Fatalf("non-LTS CheckUpgrade: %v", err)
	}
	hasWarn = false
	for _, f := range plan.Findings {
		if f.Level == "warning" && strings.Contains(f.Message, "LTS") {
			hasWarn = true
		}
	}
	if !hasWarn {
		t.Fatalf("non-LTS target must emit warning: %+v", plan.Findings)
	}

	// 目的の版のランタイムが未導入なら error finding (runtime_not_installed) を返す
	plan, err = m.CheckUpgrade(context.Background(), &pb.CheckUpgradeRequest{Instance: i, ToVersion: "26.8.1.1"})
	if err != nil {
		t.Fatalf("missing runtime CheckUpgrade: %v", err)
	}
	hasMissing := false
	for _, f := range plan.Findings {
		if f.Level == "error" && f.Code == "runtime_not_installed" {
			hasMissing = true
		}
	}
	if !hasMissing {
		t.Fatalf("missing target runtime must emit runtime_not_installed error finding: %+v", plan.Findings)
	}

	// ダウングレードは error finding (upgrade_path) を返す
	plan, err = m.CheckUpgrade(context.Background(), &pb.CheckUpgradeRequest{Instance: iCur, ToVersion: "25.8.1.1"})
	if err != nil {
		t.Fatalf("downgrade CheckUpgrade: %v", err)
	}
	hasPathErr := false
	for _, f := range plan.Findings {
		if f.Level == "error" && f.Code == "upgrade_path" {
			hasPathErr = true
		}
	}
	if !hasPathErr {
		t.Fatalf("downgrade must emit upgrade_path error finding: %+v", plan.Findings)
	}

	// データディレクトリが空または読めなければエラー
	iEmptyDir := cloneInstance(i)
	iEmptyDir.DataDir = ""
	if _, err := m.CheckUpgrade(context.Background(), &pb.CheckUpgradeRequest{Instance: iEmptyDir, ToVersion: "26.3.19.3"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("empty data dir: %v", err)
	}
	iBadDir := cloneInstance(i)
	iBadDir.DataDir = filepath.Join(t.TempDir(), "missing")
	if _, err := m.CheckUpgrade(context.Background(), &pb.CheckUpgradeRequest{Instance: iBadDir, ToVersion: "26.3.19.3"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unreadable data dir: %v", err)
	}

	// RuntimeDir や Version が空でもパニックせず未導入エラーと info を返す
	iMinimal := cloneInstance(i)
	iMinimal.Version = ""
	iMinimal.RuntimeDir = ""
	plan, err = m.CheckUpgrade(context.Background(), &pb.CheckUpgradeRequest{Instance: iMinimal, ToVersion: "26.3.19.3"})
	if err != nil {
		t.Fatalf("minimal CheckUpgrade: %v", err)
	}
	hasMissing, hasCurrent := false, false
	for _, f := range plan.Findings {
		if f.Level == "error" && f.Code == "runtime_not_installed" {
			hasMissing = true
		}
		if f.Level == "info" && strings.Contains(f.Message, "current") {
			hasCurrent = true
		}
	}
	if !hasMissing || !hasCurrent {
		t.Fatalf("expected runtime_not_installed and current label: %+v", plan.Findings)
	}

	// 引数不正
	if _, err := m.CheckUpgrade(context.Background(), &pb.CheckUpgradeRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty request: %v", err)
	}
	if _, err := m.CheckUpgrade(context.Background(), &pb.CheckUpgradeRequest{Instance: i, ToVersion: "bad"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad to_version: %v", err)
	}
	if _, err := m.CheckUpgrade(context.Background(), &pb.CheckUpgradeRequest{Instance: &pb.InstanceSpec{Id: "x"}, ToVersion: "26.3.19.3"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no port: %v", err)
	}

	// サーバー停止中なら Unavailable
	srv.Close()
	if _, err := m.CheckUpgrade(context.Background(), &pb.CheckUpgradeRequest{Instance: i, ToVersion: "26.3.19.3"}); status.Code(err) != codes.Unavailable {
		t.Fatalf("server down: %v", err)
	}
}

func TestStartUpgraded_新しい版のランタイムでPrepareStartと同じ設定と起動計画を返す(t *testing.T) {
	m := newModule()
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	i := instanceFor(t, srv)
	i.Version = "26.3.19.3"
	i.Bind = "0.0.0.0"
	i.RuntimeDir = filepath.Join(t.TempDir(), "26.3.19.3")
	bin := filepath.Join(i.RuntimeDir, "usr", "bin", "clickhouse")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	resp, err := m.StartUpgraded(context.Background(), &pb.StartUpgradedRequest{
		Instance: i,
		Login:    &pb.Login{User: "app", Password: "secret"},
	})
	if err != nil {
		t.Fatalf("StartUpgraded: %v", err)
	}
	plan := resp.GetPlan()
	if plan.GetCommand() != bin || len(plan.Args) != 2 || plan.Args[0] != "server" ||
		plan.Args[1] != "--config-file="+filepath.Join(i.StateDir, "config.xml") ||
		plan.ServiceType != "simple" || plan.KillSignal != "SIGTERM" || plan.KillMode != "mixed" || plan.TimeoutStopSec != 120 {
		t.Fatalf("unexpected LaunchPlan: %+v", plan)
	}
	if len(resp.Findings) == 0 || resp.Findings[0].Level != "info" || resp.Findings[0].Code != "log_upgrade" {
		t.Fatalf("unexpected upgrade findings: %+v", resp.Findings)
	}
	cfg, err := os.ReadFile(filepath.Join(i.StateDir, "config.xml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"tcp_port", "mysql_port", "postgresql_port"} {
		if strings.Contains(string(cfg), forbidden) {
			t.Errorf("config.xml must open HTTP only, found %s", forbidden)
		}
	}
	users, err := os.ReadFile(filepath.Join(i.StateDir, "users.xml"))
	if err != nil {
		t.Fatal(err)
	}
	ux := string(users)
	if !strings.Contains(ux, "<admin>") || !strings.Contains(ux, "<app>") ||
		!strings.Contains(ux, hashPassword("secret")) || strings.Contains(ux, "secret") ||
		!strings.Contains(ux, loginGrants) {
		t.Fatalf("users.xml violated security rules:\n%s", ux)
	}

	// Login が空でも既存の logins.json を壊さずそのまま起動計画を返せる
	iNoVer := cloneInstance(i)
	iNoVer.Version = ""
	resp2, err := m.StartUpgraded(context.Background(), &pb.StartUpgradedRequest{Instance: iNoVer})
	if err != nil || resp2.GetPlan().GetCommand() != bin {
		t.Fatalf("StartUpgraded without login: %+v %v", resp2, err)
	}
	users2, _ := os.ReadFile(filepath.Join(i.StateDir, "users.xml"))
	if !strings.Contains(string(users2), hashPassword("secret")) {
		t.Fatalf("existing login must be preserved:\n%s", users2)
	}

	// 引数不正 (nil / ポート未割当) は InvalidArgument
	if _, err := m.StartUpgraded(context.Background(), &pb.StartUpgradedRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("nil instance: %v", err)
	}
	iBad := cloneInstance(i)
	iBad.Ports = nil
	if _, err := m.StartUpgraded(context.Background(), &pb.StartUpgradedRequest{Instance: iBad}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no port: %v", err)
	}
}
