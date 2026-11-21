package main

import (
	"bytes"
	"context"
	"encoding/json"
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
		!d.Capabilities.SourceDump || d.Capabilities.BranchMerge || d.Capabilities.HistoryRecord ||
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

func hostPortOf(t *testing.T, srv *httptest.Server) (string, int32) {
	t.Helper()
	h, p, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	pn, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return h, int32(pn)
}

func jsonCompact(cols []string, rows [][]any) string {
	meta := make([]map[string]string, len(cols))
	for i, c := range cols {
		meta[i] = map[string]string{"name": c}
	}
	b, _ := json.Marshal(map[string]any{"meta": meta, "data": rows, "rows": len(rows)})
	return string(b)
}

func TestDumpSourceとLoad_ソースから読み取り専用で0600ダンプを書き出し別インスタンスへ復元できる(t *testing.T) {
	m := newModule()
	m.dumpChunkSize = 1024

	eventsNative := bytes.Repeat([]byte{0x00, 0x01, 0x7f, 0xfe, 'A', 'B'}, 1500)
	notesNative := []byte("native-notes-payload-\x00\xff")
	mvInnerNative := []byte("native-mv-inner-payload")

	ddls := map[string]string{
		"`app`.`events`":     "CREATE TABLE app.events (`id` UInt64, `url` String) ENGINE = MergeTree ORDER BY id",
		"`app`.`mem`":        "CREATE TABLE app.mem (`k` String) ENGINE = Memory",
		"`app`.`v_events`":   "CREATE VIEW app.v_events AS SELECT id, url FROM app.events",
		"`app`.`mv_to`":      "CREATE MATERIALIZED VIEW app.mv_to TO app.mem (`k` String) AS SELECT url AS k FROM app.events",
		"`app`.`mv_inner`":   "CREATE MATERIALIZED VIEW app.mv_inner (`id` UInt64) ENGINE = MergeTree ORDER BY id AS SELECT id FROM app.events",
		"`default`.`notes`":  "CREATE TABLE default.notes (`body` String) ENGINE = TinyLog",
		"`default`.`v_note`": "CREATE VIEW default.v_note AS SELECT body FROM default.notes",
	}

	var srcSQLs []string
	srcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "srcuser" || p != "srcsecret" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, "Code: 516. Authentication failed")
			return
		}
		b, _ := io.ReadAll(r.Body)
		sql := strings.TrimSpace(string(b))
		srcSQLs = append(srcSQLs, sql)
		upper := strings.ToUpper(sql)
		if !strings.HasPrefix(upper, "SELECT ") && !strings.HasPrefix(upper, "SHOW CREATE TABLE ") {
			t.Errorf("DumpSource must only send read-only SQL, got: %s", sql)
		}
		switch {
		case sql == "SELECT version()":
			io.WriteString(w, jsonCompact([]string{"version()"}, [][]any{{"26.3.19.3"}}))
		case strings.Contains(sql, "FROM system.databases"):
			io.WriteString(w, jsonCompact([]string{"name", "engine"}, [][]any{{"app", "Atomic"}, {"default", "Atomic"}}))
		case strings.Contains(sql, "FROM system.tables"):
			io.WriteString(w, jsonCompact([]string{"database", "name", "engine"}, [][]any{
				{"app", "events", "MergeTree"},
				{"app", "mem", "Memory"},
				{"app", "mv_inner", "MaterializedView"},
				{"app", "mv_to", "MaterializedView"},
				{"app", "v_events", "View"},
				{"default", "notes", "TinyLog"},
				{"default", "v_note", "View"},
			}))
		case strings.HasPrefix(sql, "SHOW CREATE TABLE "):
			target := strings.TrimPrefix(sql, "SHOW CREATE TABLE ")
			ddl, ok := ddls[target]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, "Code: 60. Unknown table "+target)
				return
			}
			io.WriteString(w, jsonCompact([]string{"statement"}, [][]any{{ddl}}))
		case sql == "SELECT * FROM `app`.`events` FORMAT Native":
			_, _ = w.Write(eventsNative)
		case sql == "SELECT * FROM `app`.`mem` FORMAT Native":
			// empty table: 0 bytes
		case sql == "SELECT * FROM `default`.`notes` FORMAT Native":
			_, _ = w.Write(notesNative)
		case sql == "SELECT * FROM `app`.`mv_inner` FORMAT Native":
			_, _ = w.Write(mvInnerNative)
		default:
			t.Errorf("unexpected source SQL: %s", sql)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srcSrv.Close()

	srcHost, srcPort := hostPortOf(t, srcSrv)
	dumpPath := filepath.Join(t.TempDir(), "dump.chb")
	_, err := m.DumpSource(context.Background(), &pb.DumpSourceRequest{
		Source: &pb.RefreshSource{
			Host: srcHost, Port: srcPort, User: "srcuser", Password: "srcsecret",
			Databases: []string{"app", "default"},
		},
		OutPath: dumpPath,
	})
	if err != nil {
		t.Fatalf("DumpSource: %v", err)
	}

	fi, err := os.Stat(dumpPath)
	if err != nil {
		t.Fatalf("stat dump: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("dump file mode = %04o, want 0600", fi.Mode().Perm())
	}

	// Load into a mock target ClickHouse instance.
	var dstExecs []string
	inserts := map[string][]byte{}
	dstSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "admin" || p != "" {
			t.Errorf("Load must authenticate as admin, got %q/%q (ok=%v)", u, p, ok)
		}
		if q := r.URL.Query().Get("query"); q != "" {
			body, _ := io.ReadAll(r.Body)
			dstExecs = append(dstExecs, q)
			inserts[q] = body
			return
		}
		b, _ := io.ReadAll(r.Body)
		dstExecs = append(dstExecs, strings.TrimSpace(string(b)))
	}))
	defer dstSrv.Close()

	dstInst := instanceFor(t, dstSrv)
	if _, err := m.Load(context.Background(), &pb.LoadRequest{Instance: dstInst, DumpPath: dumpPath}); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Verify DB setup: non-default dropped & recreated; default created without dropping DB.
	joined := strings.Join(dstExecs, "\n")
	if !strings.Contains(joined, "DROP DATABASE IF EXISTS `app` SYNC") ||
		!strings.Contains(joined, "CREATE DATABASE IF NOT EXISTS `app`") ||
		strings.Contains(joined, "DROP DATABASE IF EXISTS `default`") ||
		!strings.Contains(joined, "CREATE DATABASE IF NOT EXISTS `default`") ||
		!strings.Contains(joined, "DROP TABLE IF EXISTS `default`.`notes`") ||
		!strings.Contains(joined, "DROP VIEW IF EXISTS `default`.`v_note`") {
		t.Fatalf("unexpected target statements:\n%s", joined)
	}

	// Verify base table data was inserted BEFORE views and materialized views were created.
	idxEventsInsert := strings.Index(joined, "INSERT INTO `app`.`events` FORMAT Native")
	idxMVCreate := strings.Index(joined, "CREATE MATERIALIZED VIEW app.mv_to")
	idxViewCreate := strings.Index(joined, "CREATE VIEW app.v_events")
	if idxEventsInsert < 0 || idxMVCreate < 0 || idxViewCreate < 0 || idxEventsInsert > idxMVCreate || idxEventsInsert > idxViewCreate {
		t.Fatalf("base tables must be loaded before views/MVs:\n%s", joined)
	}

	// Verify Native payloads arrived byte-for-byte intact and empty/view/TO-MV tables were not inserted.
	if !bytes.Equal(inserts["INSERT INTO `app`.`events` FORMAT Native"], eventsNative) {
		t.Fatalf("events native payload mismatch (got %d bytes)", len(inserts["INSERT INTO `app`.`events` FORMAT Native"]))
	}
	if !bytes.Equal(inserts["INSERT INTO `default`.`notes` FORMAT Native"], notesNative) {
		t.Fatalf("notes native payload mismatch")
	}
	if !bytes.Equal(inserts["INSERT INTO `app`.`mv_inner` FORMAT Native"], mvInnerNative) {
		t.Fatalf("mv_inner native payload mismatch")
	}
	if len(inserts) != 3 {
		t.Fatalf("expected 3 INSERTs (events, notes, mv_inner), got %d: %v", len(inserts), dstExecs)
	}
}

func TestDumpSource_システムDBや外部エンジンや危険な関数を含むテーブルは全体をエラーで中止し部分ファイルを残さない(t *testing.T) {
	m := newModule()
	ctx := context.Background()

	// 1. Forbidden system databases are rejected even if explicitly requested.
	for _, sysDB := range []string{"system", "INFORMATION_SCHEMA", "information_schema", "SYSTEM"} {
		out := filepath.Join(t.TempDir(), "out.chb")
		_, err := m.DumpSource(ctx, &pb.DumpSourceRequest{
			Source:  &pb.RefreshSource{Host: "127.0.0.1", Port: 8123, User: "u", Databases: []string{"app", sysDB}},
			OutPath: out,
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("db %q: got %v, want InvalidArgument", sysDB, err)
		}
		if _, statErr := os.Stat(out); statErr == nil {
			t.Errorf("db %q: partial file must not exist", sysDB)
		}
	}

	// 2. Source containing external engines & dangerous functions aborts entire dump and lists ALL offending tables.
	srcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sql := strings.TrimSpace(string(b))
		switch {
		case sql == "SELECT version()":
			io.WriteString(w, jsonCompact([]string{"version()"}, [][]any{{"26.3.19.3"}}))
		case strings.Contains(sql, "FROM system.databases"):
			io.WriteString(w, jsonCompact([]string{"name", "engine"}, [][]any{{"app", "Atomic"}}))
		case strings.Contains(sql, "FROM system.tables"):
			io.WriteString(w, jsonCompact([]string{"database", "name", "engine"}, [][]any{
				{"app", "ok_tbl", "MergeTree"},
				{"app", "ext_url", "URL"},
				{"app", "ext_s3", "S3"},
				{"app", "ext_kafka", "Kafka"},
				{"app", "ext_dict", "Dictionary"},
				{"app", "v_file", "View"},
			}))
		case sql == "SHOW CREATE TABLE `app`.`ok_tbl`":
			io.WriteString(w, jsonCompact([]string{"statement"}, [][]any{{"CREATE TABLE app.ok_tbl (`id` UInt64) ENGINE = MergeTree ORDER BY id"}}))
		case sql == "SHOW CREATE TABLE `app`.`v_file`":
			io.WriteString(w, jsonCompact([]string{"statement"}, [][]any{{"CREATE VIEW app.v_file AS SELECT * FROM FILE('/etc/passwd', 'LineAsString')"}}))
		default:
			t.Errorf("unexpected query after unsupported tables should have aborted: %s", sql)
		}
	}))
	defer srcSrv.Close()

	h, p := hostPortOf(t, srcSrv)
	out := filepath.Join(t.TempDir(), "aborted.chb")
	_, err := m.DumpSource(ctx, &pb.DumpSourceRequest{
		Source:  &pb.RefreshSource{Host: h, Port: p, User: "u", Databases: []string{"app"}},
		OutPath: out,
	})
	if err == nil {
		t.Fatal("expected error for unsupported engines/functions")
	}
	msg := err.Error()
	for _, want := range []string{"app.ext_url", "URL", "app.ext_s3", "S3", "app.ext_kafka", "Kafka", "app.ext_dict", "Dictionary", "app.v_file", "FILE"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must list %q, got: %s", want, msg)
		}
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Fatal("partial dump file must not exist on abort")
	}

	// 3. Unsafe database engine & missing database are also rejected.
	badDBSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sql := strings.TrimSpace(string(b))
		switch {
		case sql == "SELECT version()":
			io.WriteString(w, jsonCompact([]string{"version()"}, [][]any{{"26.3.19.3"}}))
		case strings.Contains(sql, "FROM system.databases"):
			io.WriteString(w, jsonCompact([]string{"name", "engine"}, [][]any{{"extdb", "MySQL"}}))
		}
	}))
	defer badDBSrv.Close()
	bh, bp := hostPortOf(t, badDBSrv)
	out2 := filepath.Join(t.TempDir(), "baddb.chb")
	if _, err := m.DumpSource(ctx, &pb.DumpSourceRequest{
		Source:  &pb.RefreshSource{Host: bh, Port: bp, User: "u", Databases: []string{"extdb"}},
		OutPath: out2,
	}); err == nil || !strings.Contains(err.Error(), "MySQL") {
		t.Fatalf("unsafe database engine: %v", err)
	}
	if _, err := m.DumpSource(ctx, &pb.DumpSourceRequest{
		Source:  &pb.RefreshSource{Host: bh, Port: bp, User: "u", Databases: []string{"missing"}},
		OutPath: out2,
	}); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing database: %v", err)
	}
}

func TestDumpSource_サイズ上限やテーブル数上限やリダイレクトや無通信タイムアウトを拒否しパスワードを漏らさず部分ファイルを消す(t *testing.T) {
	ctx := context.Background()

	// Conformance probe & invalid args must return InvalidArgument (not Unimplemented).
	m := newModule()
	for _, req := range []*pb.DumpSourceRequest{
		{},
		{Source: &pb.RefreshSource{}},
		{Source: &pb.RefreshSource{Host: "127.0.0.1", Port: 8123, Databases: []string{"app"}}, OutPath: ""},
		{Source: &pb.RefreshSource{Host: "bad host", Port: 8123, Databases: []string{"app"}}, OutPath: "/tmp/x"},
		{Source: &pb.RefreshSource{Host: "127.0.0.1", Port: 0, Databases: []string{"app"}}, OutPath: "/tmp/x"},
		{Source: &pb.RefreshSource{Host: "127.0.0.1", Port: 8123, Databases: []string{"app", "app"}}, OutPath: "/tmp/x"},
		{Source: &pb.RefreshSource{Host: "127.0.0.1", Port: 8123, Databases: []string{"bad`db"}}, OutPath: "/tmp/x"},
	} {
		if _, err := m.DumpSource(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("req %+v: got %v, want InvalidArgument", req, err)
		}
	}

	// Redirect must NOT be followed.
	redirectHit := false
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer targetSrv.Close()
	redirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targetSrv.URL, http.StatusFound)
	}))
	defer redirSrv.Close()
	rh, rp := hostPortOf(t, redirSrv)
	outRedir := filepath.Join(t.TempDir(), "redir.chb")
	if _, err := m.DumpSource(ctx, &pb.DumpSourceRequest{
		Source:  &pb.RefreshSource{Host: rh, Port: rp, User: "u", Password: "super-secret-password", Databases: []string{"app"}},
		OutPath: outRedir,
	}); err == nil {
		t.Fatal("redirect must fail")
	} else if strings.Contains(err.Error(), "super-secret-password") {
		t.Fatalf("password leaked in error: %v", err)
	}
	if redirectHit {
		t.Fatal("DumpSource followed an HTTP redirect")
	}

	// Password echoed by server in error body must be scrubbed.
	echoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "Code: 516. Auth failed for password=top-secret-pw-999")
	}))
	defer echoSrv.Close()
	eh, ep := hostPortOf(t, echoSrv)
	if _, err := m.DumpSource(ctx, &pb.DumpSourceRequest{
		Source:  &pb.RefreshSource{Host: eh, Port: ep, User: "u", Password: "top-secret-pw-999", Databases: []string{"app"}},
		OutPath: filepath.Join(t.TempDir(), "echo.chb"),
	}); err == nil || strings.Contains(err.Error(), "top-secret-pw-999") {
		t.Fatalf("password must be scrubbed from error: %v", err)
	}

	// Size limit and table count limit leave NO partial file behind.
	var mode string
	limSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sql := strings.TrimSpace(string(b))
		switch {
		case sql == "SELECT version()":
			io.WriteString(w, jsonCompact([]string{"version()"}, [][]any{{"26.3.19.3"}}))
		case strings.Contains(sql, "FROM system.databases"):
			io.WriteString(w, jsonCompact([]string{"name", "engine"}, [][]any{{"app", "Atomic"}}))
		case strings.Contains(sql, "FROM system.tables"):
			if mode == "many" {
				io.WriteString(w, jsonCompact([]string{"database", "name", "engine"}, [][]any{
					{"app", "t1", "Memory"},
					{"app", "t2", "Memory"},
					{"app", "t3", "Memory"},
				}))
				return
			}
			io.WriteString(w, jsonCompact([]string{"database", "name", "engine"}, [][]any{{"app", "big", "Memory"}}))
		case sql == "SHOW CREATE TABLE `app`.`big`":
			io.WriteString(w, jsonCompact([]string{"statement"}, [][]any{{"CREATE TABLE app.big (`x` String) ENGINE = Memory"}}))
		case sql == "SELECT * FROM `app`.`big` FORMAT Native":
			if mode == "stall" {
				w.WriteHeader(http.StatusOK)
				if fl, ok := w.(http.Flusher); ok {
					fl.Flush()
				}
				time.Sleep(400 * time.Millisecond)
				return
			}
			_, _ = w.Write(bytes.Repeat([]byte("X"), 4096))
		}
	}))
	defer limSrv.Close()
	lh, lp := hostPortOf(t, limSrv)

	// Table count limit
	mLim := newModule()
	mLim.maxDumpTables = 2
	mode = "many"
	outMany := filepath.Join(t.TempDir(), "many.chb")
	if _, err := mLim.DumpSource(ctx, &pb.DumpSourceRequest{
		Source:  &pb.RefreshSource{Host: lh, Port: lp, User: "u", Databases: []string{"app"}},
		OutPath: outMany,
	}); err == nil {
		t.Fatal("exceeding table limit must fail")
	}
	if _, err := os.Stat(outMany); err == nil {
		t.Fatal("partial file left after table limit error")
	}

	// Byte size limit (even when outPath was pre-created like srcdump.Dumper does)
	mLim.maxDumpTables = 100
	mLim.maxDumpBytes = 512
	mode = "size"
	outBig := filepath.Join(t.TempDir(), "big.chb")
	_ = os.WriteFile(outBig, nil, 0o600)
	if _, err := mLim.DumpSource(ctx, &pb.DumpSourceRequest{
		Source:  &pb.RefreshSource{Host: lh, Port: lp, User: "u", Databases: []string{"app"}},
		OutPath: outBig,
	}); err == nil {
		t.Fatal("exceeding byte limit must fail")
	}
	if _, err := os.Stat(outBig); err == nil {
		t.Fatal("partial file must be removed when byte limit is exceeded")
	}

	// Idle data timeout
	mStall := newModule()
	mStall.dumpIdleTimeout = 80 * time.Millisecond
	mode = "stall"
	outStall := filepath.Join(t.TempDir(), "stall.chb")
	if _, err := mStall.DumpSource(ctx, &pb.DumpSourceRequest{
		Source:  &pb.RefreshSource{Host: lh, Port: lp, User: "u", Databases: []string{"app"}},
		OutPath: outStall,
	}); err == nil {
		t.Fatal("stalled stream must time out")
	}
	if _, err := os.Stat(outStall); err == nil {
		t.Fatal("partial file must be removed on stream timeout")
	}
}

func TestLoad_改ざんされたダンプや未知の版や途中エラーを安全に拒否する(t *testing.T) {
	m := newModule()
	ctx := context.Background()

	var executed []string
	failOn := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sql := r.URL.Query().Get("query")
		if sql == "" {
			b, _ := io.ReadAll(r.Body)
			sql = strings.TrimSpace(string(b))
		} else {
			_, _ = io.Copy(io.Discard, r.Body)
		}
		executed = append(executed, sql)
		if failOn != "" && strings.Contains(sql, failOn) {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "Code: 241. DB::Exception: Memory limit exceeded\nStack trace:\n0. frame\n")
		}
	}))
	defer srv.Close()
	i := instanceFor(t, srv)

	// Invalid arguments
	if _, err := m.Load(ctx, &pb.LoadRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty Load: %v", err)
	}
	if _, err := m.Load(ctx, &pb.LoadRequest{Instance: &pb.InstanceSpec{Id: "x"}, DumpPath: "/tmp/x"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no port Load: %v", err)
	}
	if _, err := m.Load(ctx, &pb.LoadRequest{Instance: i, DumpPath: filepath.Join(t.TempDir(), "missing.chb")}); err == nil {
		t.Fatal("missing dump file must fail")
	}

	// Helper to write a single-table dump with an arbitrary DDL (bypassing DumpSource checks).
	writeRawDump := func(hdr dumpHeader, ddl string, payload []byte) string {
		p := filepath.Join(t.TempDir(), "tampered.chb")
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		w := newDumpWriter(f, 1<<20)
		if err := w.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if len(hdr.Tables) > 0 {
			var dr io.Reader
			if hdr.Tables[0].HasData {
				dr = bytes.NewReader(payload)
			}
			if err := w.WriteTable(hdr.Tables[0], ddl, dr, 1024); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		return p
	}

	// Tampered DDLs must be rejected BEFORE sending the DDL to ClickHouse.
	tamperedCases := []struct {
		name string
		hdr  dumpHeader
		ddl  string
	}{
		{
			name: "未知の版",
			hdr:  dumpHeader{Format: dumpFormatMagic, Version: 2, Databases: []string{"app"}},
		},
		{
			name: "DDLのENGINEをURLに改ざん",
			hdr: dumpHeader{
				Format: dumpFormatMagic, Version: 1, Databases: []string{"app"},
				Tables: []dumpTableMeta{{Database: "app", Name: "t", Engine: "MergeTree", HasData: false}},
			},
			ddl: "CREATE TABLE app.t (`x` String) ENGINE = URL('http://127.0.0.1/', 'LineAsString')",
		},
		{
			name: "Viewにurl関数を混入",
			hdr: dumpHeader{
				Format: dumpFormatMagic, Version: 1, Databases: []string{"app"},
				Tables: []dumpTableMeta{{Database: "app", Name: "v", Engine: "View", HasData: false}},
			},
			ddl: "CREATE VIEW app.v AS SELECT * FROM url('http://169.254.169.254/', 'LineAsString')",
		},
		{
			name: "セミコロンで2文目を注入",
			hdr: dumpHeader{
				Format: dumpFormatMagic, Version: 1, Databases: []string{"app"},
				Tables: []dumpTableMeta{{Database: "app", Name: "t", Engine: "Memory", HasData: false}},
			},
			ddl: "CREATE TABLE app.t (`x` UInt8) ENGINE = Memory; CREATE USER evil IDENTIFIED BY 'p'",
		},
	}
	for _, tc := range tamperedCases {
		executed = nil
		p := writeRawDump(tc.hdr, tc.ddl, nil)
		if _, err := m.Load(ctx, &pb.LoadRequest{Instance: i, DumpPath: p}); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
		for _, sql := range executed {
			if strings.HasPrefix(sql, "CREATE TABLE") || strings.HasPrefix(sql, "CREATE VIEW") || strings.Contains(sql, "CREATE USER") {
				t.Errorf("%s: tampered DDL must never be sent to ClickHouse: %s", tc.name, sql)
			}
		}
	}

	// ClickHouse error during DDL or INSERT includes table name and Code + short message (no stack trace).
	goodHdr := dumpHeader{
		Format: dumpFormatMagic, Version: 1, Databases: []string{"app"},
		Tables: []dumpTableMeta{{Database: "app", Name: "events", Engine: "MergeTree", HasData: true}},
	}
	goodDump := writeRawDump(goodHdr, "CREATE TABLE app.events (`id` UInt64) ENGINE = MergeTree ORDER BY id", []byte("native-bytes"))

	failOn = "CREATE TABLE app.events"
	_, err := m.Load(ctx, &pb.LoadRequest{Instance: i, DumpPath: goodDump})
	if err == nil || !strings.Contains(err.Error(), "app.events") || !strings.Contains(err.Error(), "Code: 241.") || strings.Contains(err.Error(), "Stack trace") {
		t.Fatalf("DDL failure error = %v", err)
	}

	failOn = "INSERT INTO `app`.`events`"
	_, err = m.Load(ctx, &pb.LoadRequest{Instance: i, DumpPath: goodDump})
	if err == nil || !strings.Contains(err.Error(), "app.events") || !strings.Contains(err.Error(), "Code: 241.") || strings.Contains(err.Error(), "Stack trace") {
		t.Fatalf("INSERT failure error = %v", err)
	}

	// DB-level errors (DROP DATABASE / CREATE DATABASE) and default-DB DROP TABLE error
	failOn = "DROP DATABASE"
	if _, err := m.Load(ctx, &pb.LoadRequest{Instance: i, DumpPath: goodDump}); err == nil || !strings.Contains(err.Error(), "DROP DATABASE") {
		t.Fatalf("DROP DATABASE failure: %v", err)
	}
	failOn = "CREATE DATABASE"
	if _, err := m.Load(ctx, &pb.LoadRequest{Instance: i, DumpPath: goodDump}); err == nil || !strings.Contains(err.Error(), "CREATE DATABASE") {
		t.Fatalf("CREATE DATABASE failure: %v", err)
	}
	defHdr := dumpHeader{
		Format: dumpFormatMagic, Version: 1, Databases: []string{"default"},
		Tables: []dumpTableMeta{{Database: "default", Name: "t", Engine: "Memory", HasData: false}},
	}
	defDump := writeRawDump(defHdr, "CREATE TABLE default.t (`x` UInt8) ENGINE = Memory", nil)
	failOn = "DROP TABLE IF EXISTS `default`.`t`"
	if _, err := m.Load(ctx, &pb.LoadRequest{Instance: i, DumpPath: defDump}); err == nil || !strings.Contains(err.Error(), "default.t") {
		t.Fatalf("default DROP TABLE failure: %v", err)
	}
	failOn = ""

	// Directory path, truncated dump, trailing garbage, and server down
	if _, err := m.Load(ctx, &pb.LoadRequest{Instance: i, DumpPath: t.TempDir()}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("directory dump path: %v", err)
	}
	rawGood, _ := os.ReadFile(goodDump)
	truncPath := filepath.Join(t.TempDir(), "trunc.chb")
	_ = os.WriteFile(truncPath, rawGood[:len(rawGood)-5], 0o600)
	if _, err := m.Load(ctx, &pb.LoadRequest{Instance: i, DumpPath: truncPath}); err == nil {
		t.Fatal("truncated dump must fail Load")
	}
	trailPath := filepath.Join(t.TempDir(), "trail.chb")
	_ = os.WriteFile(trailPath, append(append([]byte(nil), rawGood...), "junk"...), 0o600)
	if _, err := m.Load(ctx, &pb.LoadRequest{Instance: i, DumpPath: trailPath}); err == nil {
		t.Fatal("trailing garbage dump must fail Load")
	}
	srv.Close()
	if _, err := m.Load(ctx, &pb.LoadRequest{Instance: i, DumpPath: goodDump}); err == nil {
		t.Fatal("unreachable target must fail Load")
	}
}

func TestDumpSource_異常応答やファイル作成失敗やNullエンジンを正しく扱う(t *testing.T) {
	ctx := context.Background()
	m := newModule()

	// Empty host, empty databases, & Quiesce coverage
	if _, err := m.DumpSource(ctx, &pb.DumpSourceRequest{
		Source:  &pb.RefreshSource{Host: "", Port: 8123, Databases: []string{"app"}},
		OutPath: "/tmp/x",
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty host: %v", err)
	}
	if _, err := m.DumpSource(ctx, &pb.DumpSourceRequest{
		Source:  &pb.RefreshSource{Host: "127.0.0.1", Port: 8123},
		OutPath: "/tmp/x",
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty databases: %v", err)
	}
	if _, err := m.Quiesce(ctx, &pb.QuiesceRequest{}); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}

	scenario := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sql := strings.TrimSpace(string(b))
		switch {
		case sql == "SELECT version()":
			if scenario == "bad_ver_json" {
				io.WriteString(w, "not json")
				return
			}
			io.WriteString(w, jsonCompact([]string{"version()"}, [][]any{{"26.3.19.3"}}))
		case strings.Contains(sql, "FROM system.databases"):
			if scenario == "db_http_err" {
				w.WriteHeader(http.StatusInternalServerError)
				io.WriteString(w, "Code: 81. DB error")
				return
			}
			io.WriteString(w, jsonCompact([]string{"name", "engine"}, [][]any{{"app", "Atomic"}}))
		case strings.Contains(sql, "FROM system.tables"):
			switch scenario {
			case "tbl_http_err":
				w.WriteHeader(http.StatusInternalServerError)
				io.WriteString(w, "Code: 60. Table list error")
				return
			case "bad_tbl_row":
				io.WriteString(w, jsonCompact([]string{"database"}, [][]any{{"app"}}))
			case "unexpected_db":
				io.WriteString(w, jsonCompact([]string{"database", "name", "engine"}, [][]any{{"system", "users", "Memory"}}))
			case "bad_tbl_ident":
				io.WriteString(w, jsonCompact([]string{"database", "name", "engine"}, [][]any{{"app", "bad`name", "Memory"}}))
			case "null_engine":
				io.WriteString(w, jsonCompact([]string{"database", "name", "engine"}, [][]any{{"app", "sink", "Null"}}))
			default:
				io.WriteString(w, jsonCompact([]string{"database", "name", "engine"}, [][]any{{"app", "t", "Memory"}}))
			}
		case strings.HasPrefix(sql, "SHOW CREATE TABLE "):
			switch scenario {
			case "ddl_http_err":
				w.WriteHeader(http.StatusInternalServerError)
				io.WriteString(w, "Code: 60. Unknown table")
			case "ddl_empty":
				io.WriteString(w, jsonCompact([]string{"statement"}, [][]any{}))
			case "null_engine":
				io.WriteString(w, jsonCompact([]string{"statement"}, [][]any{{"CREATE TABLE app.sink (`x` UInt8) ENGINE = Null"}}))
			default:
				io.WriteString(w, jsonCompact([]string{"statement"}, [][]any{{"CREATE TABLE app.t (`x` UInt8) ENGINE = Memory"}}))
			}
		case strings.Contains(sql, "FORMAT Native"):
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "Code: 159. Timeout exceeded for secret-pw")
		}
	}))
	defer srv.Close()
	h, p := hostPortOf(t, srv)

	for _, sc := range []string{"bad_ver_json", "db_http_err", "tbl_http_err", "bad_tbl_row", "unexpected_db", "bad_tbl_ident", "ddl_http_err", "ddl_empty", "native_err"} {
		scenario = sc
		out := filepath.Join(t.TempDir(), sc+".chb")
		_, err := m.DumpSource(ctx, &pb.DumpSourceRequest{
			Source:  &pb.RefreshSource{Host: h, Port: p, User: "u", Password: "secret-pw", Databases: []string{"app"}},
			OutPath: out,
		})
		if err == nil {
			t.Errorf("scenario %s: expected error", sc)
		} else if strings.Contains(err.Error(), "secret-pw") {
			t.Errorf("scenario %s: password leaked: %v", sc, err)
		}
		if _, statErr := os.Stat(out); statErr == nil {
			t.Errorf("scenario %s: partial file must not remain", sc)
		}
	}

	// Null engine succeeds without issuing SELECT * FORMAT Native, and zero-initialized defaults work too.
	scenario = "null_engine"
	outNull := filepath.Join(t.TempDir(), "null.chb")
	mZero := &module{}
	if _, err := mZero.DumpSource(ctx, &pb.DumpSourceRequest{
		Source:  &pb.RefreshSource{Host: h, Port: p, User: "u", Databases: []string{"app"}},
		OutPath: outNull,
	}); err != nil {
		t.Fatalf("Null engine dump failed: %v", err)
	}

	// Size limit tripped during header write and during HasData=false table write removes partial file.
	for _, limit := range []int64{10, 200} {
		mTiny := newModule()
		mTiny.maxDumpBytes = limit
		outTiny := filepath.Join(t.TempDir(), "tiny.chb")
		if _, err := mTiny.DumpSource(ctx, &pb.DumpSourceRequest{
			Source:  &pb.RefreshSource{Host: h, Port: p, User: "u", Databases: []string{"app"}},
			OutPath: outTiny,
		}); status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("limit %d: got %v, want ResourceExhausted", limit, err)
		}
		if _, statErr := os.Stat(outTiny); statErr == nil {
			t.Fatalf("limit %d: partial file must not remain", limit)
		}
	}

	// Unwritable outPath fails cleanly.
	if _, err := m.DumpSource(ctx, &pb.DumpSourceRequest{
		Source:  &pb.RefreshSource{Host: h, Port: p, User: "u", Databases: []string{"app"}},
		OutPath: filepath.Join(t.TempDir(), "no-such-dir", "dump.chb"),
	}); err == nil {
		t.Fatal("nonexistent output directory must fail")
	}
}
