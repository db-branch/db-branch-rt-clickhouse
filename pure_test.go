package main

import (
	"encoding/xml"
	"strconv"
	"strings"
	"testing"

	pb "github.com/db-branch/db-branch/pkg/ext/proto/runtime/v1"
)

func inst() *pb.InstanceSpec {
	return &pb.InstanceSpec{
		Id: "branch-x", RuntimeDir: "/var/lib/db-branch/runtimes/clickhouse/24.8", DataDir: "/tank/db-branch/branches/x",
		StateDir: "/var/lib/db-branch/instances/branch-x", RunDir: "/run/db-branch-rt/clickhouse/branch-x",
		Bind: "127.0.0.1", Ports: map[string]int32{"http": 18123}, Limits: &pb.ResourceLimits{MemoryMb: 1024},
	}
}

func wellFormed(t *testing.T, s string) {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(s))
	for {
		if _, err := d.Token(); err != nil {
			if err.Error() == "EOF" {
				return
			}
			t.Fatalf("not well-formed XML: %v\n%s", err, s)
		}
	}
}

func TestServerConfig_データとログを指定の場所に置きHTTPだけを開く(t *testing.T) {
	c := serverConfig(inst(), false)
	wellFormed(t, c)
	for _, want := range []string{
		"<http_port>18123</http_port>", "<listen_host>127.0.0.1</listen_host>",
		"<path>/tank/db-branch/branches/x/</path>", "<tmp_path>/tank/db-branch/branches/x/tmp/</tmp_path>",
		"<users_config>/var/lib/db-branch/instances/branch-x/users.xml</users_config>",
		"<log>/var/lib/db-branch/instances/branch-x/clickhouse-server.log</log>",
		"<max_server_memory_usage>2550136832</max_server_memory_usage>", // (1024*2+384) MiB
		"<user_files_path>/tank/db-branch/branches/x/user_files/</user_files_path>",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("config lacks %s", want)
		}
	}
	for _, not := range []string{"tcp_port", "interserver_http_port", "mysql_port", "postgresql_port", "<remote_servers"} {
		if strings.Contains(c, not) {
			t.Errorf("config opens %s", not)
		}
	}
}

func TestServerConfig_メンテナンス中は自分自身にしか待ち受けない(t *testing.T) {
	i := inst()
	i.Bind = "0.0.0.0"
	if c := serverConfig(i, true); !strings.Contains(c, "<listen_host>127.0.0.1</listen_host>") || strings.Contains(c, "0.0.0.0") {
		t.Fatalf("maintenance must listen locally only:\n%s", c)
	}
	if c := serverConfig(i, false); !strings.Contains(c, "<listen_host>0.0.0.0</listen_host>") {
		t.Fatalf("normal run listens on the bind address:\n%s", c)
	}
}

func TestServerConfig_メモリ上限が無ければ指定しない(t *testing.T) {
	i := inst()
	i.Limits = nil
	if strings.Contains(serverConfig(i, false), "max_server_memory_usage") {
		t.Fatal("no limit means no setting")
	}
}

func TestServerConfig_XMLに効く文字を含むパスは拒否される前に無害化される(t *testing.T) {
	i := inst()
	i.DataDir = "/tank/a</path><evil/>"
	c := serverConfig(i, false)
	if strings.Contains(c, "<evil/>") {
		t.Fatalf("XML injection through a path:\n%s", c)
	}
	wellFormed(t, c)
}

func TestUsersXML_管理者はローカルだけでログインは権限を絞る(t *testing.T) {
	x := usersXML(map[string]string{"app": "abc123"})
	wellFormed(t, x)
	admin := x[strings.Index(x, "<admin>"):strings.Index(x, "</admin>")]
	if !strings.Contains(admin, "<ip>127.0.0.1</ip>") || strings.Contains(admin, "::/0") {
		t.Errorf("admin must be local only:\n%s", admin)
	}
	app := x[strings.Index(x, "<app>"):strings.Index(x, "</app>")]
	if !strings.Contains(app, "<password_sha256_hex>abc123</password_sha256_hex>") {
		t.Errorf("the login stores a digest:\n%s", app)
	}
	for _, bad := range []string{"access_management", "ALL ON", "FILE", "URL", "REMOTE", "SYSTEM", "<password>"} {
		if strings.Contains(app, bad) {
			t.Errorf("the login must not have %s:\n%s", bad, app)
		}
	}
	if !strings.Contains(app, "GRANT SELECT, INSERT, ALTER, CREATE, DROP, TRUNCATE, OPTIMIZE, SHOW ON *.*") {
		t.Errorf("grants:\n%s", app)
	}
}

func TestUsersXML_ログインの順序は固定で名前はエスケープされる(t *testing.T) {
	a := usersXML(map[string]string{"b": "2", "a": "1"})
	if strings.Index(a, "<a>") > strings.Index(a, "<b>") {
		t.Fatal("logins must be in name order so the file is stable")
	}
	if x := usersXML(map[string]string{"x><evil/": "1"}); strings.Contains(x, "<evil/>") {
		t.Fatalf("name injected: %s", x)
	}
}

func TestHashPassword_SHA256の16進(t *testing.T) {
	if got := hashPassword("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("hash = %s", got)
	}
}

func TestParseResult_JSONCompactを列と行に写しNULLを区別する(t *testing.T) {
	body := `{"meta":[{"name":"a","type":"UInt8"},{"name":"b","type":"Nullable(String)"}],"data":[[1,"x"],[2,null]],"rows":2}`
	r, err := parseResult([]byte(body), 10)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.Columns, ",") != "a,b" || len(r.Rows) != 2 || r.Rows[0].Cells[0].Value != "1" || r.Rows[0].Cells[1].Value != "x" ||
		!r.Rows[1].Cells[1].IsNull || r.Truncated {
		t.Fatalf("%+v", r)
	}
}

func TestParseResult_上限で切り詰めたことを示す(t *testing.T) {
	body := `{"meta":[{"name":"a","type":"UInt8"}],"data":[[1],[2],[3]],"rows":3}`
	r, err := parseResult([]byte(body), 2)
	if err != nil || len(r.Rows) != 2 || !r.Truncated {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestParseResult_数値や配列や入れ子も文字列で返す(t *testing.T) {
	body := `{"meta":[{"name":"n","type":"Float64"},{"name":"v","type":"Array(UInt8)"},{"name":"t","type":"Bool"}],"data":[[1.5,[1,2],true]],"rows":1}`
	r, err := parseResult([]byte(body), 0)
	if err != nil {
		t.Fatal(err)
	}
	c := r.Rows[0].Cells
	if c[0].Value != "1.5" || c[1].Value != "[1,2]" || c[2].Value != "true" {
		t.Fatalf("%+v", c)
	}
}

func TestParseResult_不正な応答は失敗する(t *testing.T) {
	if _, err := parseResult([]byte("Code: 62. DB::Exception: Syntax error"), 10); err == nil {
		t.Fatal("not JSON must be an error")
	}
}

func TestSplitStatements_セミコロンで分け引用符の中は分けない(t *testing.T) {
	got := splitStatements("CREATE TABLE a (x String); INSERT INTO a VALUES ('p;q'); -- c;\nSELECT 1;")
	if len(got) != 3 || got[1] != "INSERT INTO a VALUES ('p;q')" {
		t.Fatalf("%q", got)
	}
	if got := splitStatements("  ;; \n"); len(got) != 0 {
		t.Fatalf("blank statements are dropped: %q", got)
	}
}

func TestServerConfig_ブランチのメモリ設定はキャッシュ分でサーバー全体の上限は余裕を足す(t *testing.T) {
	// ClickHouse itself needs about 140 MiB idle; the unit's MemoryMax is MemoryMB*2+512 MiB.
	i := inst()
	i.Limits = &pb.ResourceLimits{MemoryMb: 128}
	c := serverConfig(i, false)
	if !strings.Contains(c, "<max_server_memory_usage>"+strconv.FormatInt((128*2+384)<<20, 10)+"</max_server_memory_usage>") {
		t.Fatalf("a small branch still needs room for the server:\n%s", c)
	}
}
