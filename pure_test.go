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

func TestCheckUpgradePathPure_ClickHouseの4要素の版を数値で比較し新しい版だけ許可する(t *testing.T) {
	okCases := [][2]string{
		{"26.3.19.3", "26.3.19.4"},
		{"26.3.2.1", "26.3.19.3"},   // 数値比較: 2 < 19 (辞書順ではない)
		{"25.8.1.100", "26.3.19.3"}, // 隣接 LTS
		{"24.8.1.1", "26.3.19.3"},   // LTS 飛び越しでも拒否しない
		{"26.3.19.3", "26.4.1.1"},   // 非 LTS でも拒否しない
		{"1.0", "2.0"},              // 2 要素の版 (conformance プローブ)
		{"26.3", "26.3.0.1"},        // 要素数が違っても 0 詰めで比較
	}
	for _, c := range okCases {
		if err := checkUpgradePath(c[0], c[1]); err != nil {
			t.Errorf("%s -> %s: unexpected error: %v", c[0], c[1], err)
		}
	}

	badCases := [][2]string{
		{"26.3.19.3", "26.3.19.3"},                 // 同じ版
		{"26.3", "26.3.0.0"},                       // 数値的に同じ版
		{"26.3.19.3", "26.3.19.2"},                 // ダウングレード
		{"26.3.19.3", "26.3.2.99"},                 // 数値比較でダウングレード
		{"26.3.19.3", "25.8.99.99"},                // 年のダウングレード
		{"", "26.3.19.3"},                          // 空
		{"26", "26.3.19.3"},                        // 1 要素は不正
		{"26.3.19.3", "26.3.a.1"},                  // 非数値
		{"26..3", "26.4"},                          // 空セグメント
		{"26.3.19.3", "99999999999999999999999.1"}, // オーバーフロー
	}
	for _, c := range badCases {
		if err := checkUpgradePath(c[0], c[1]); err == nil {
			t.Errorf("%s -> %s: expected error, got nil", c[0], c[1])
		}
	}
}

func TestUpgradePathWarnings_LTS飛ばしや非LTS移行は警告を返し同シリーズや隣接LTSは警告しない(t *testing.T) {
	noWarn := [][2]string{
		{"26.3.1.1", "26.3.19.3"}, // 同一シリーズ内のパッチ更新
		{"25.8.1.1", "26.3.19.3"}, // 隣接 LTS (.8 -> 翌年 .3)
		{"26.3.19.3", "26.8.1.1"}, // 隣接 LTS (.3 -> 同年 .8)
	}
	for _, c := range noWarn {
		if ws := upgradePathWarnings(c[0], c[1]); len(ws) != 0 {
			t.Errorf("%s -> %s: expected no warning, got %+v", c[0], c[1], ws)
		}
	}

	// LTS を飛ばすケース (24.8 -> 26.3 は 25.3, 25.8 を飛ばす)
	ws := upgradePathWarnings("24.8.1.1", "26.3.19.3")
	if len(ws) == 0 || ws[0].Level != "warning" || ws[0].Code != "upgrade_path" ||
		!strings.Contains(ws[0].Message, "25.3") || !strings.Contains(ws[0].Message, "25.8") {
		t.Fatalf("skipping LTS must warn with skipped versions: %+v", ws)
	}

	// 多数の LTS を飛ばしても O(1) で要約して警告する
	wsMany := upgradePathWarnings("20.3.1.1", "30.8.1.1")
	if len(wsMany) == 0 || wsMany[0].Level != "warning" || !strings.Contains(wsMany[0].Message, "more") {
		t.Fatalf("many skipped LTS must be summarized: %+v", wsMany)
	}

	// 非 LTS への移行は警告する
	wsNonLTS := upgradePathWarnings("26.3.19.3", "26.4.1.1")
	if len(wsNonLTS) == 0 || wsNonLTS[0].Level != "warning" || !strings.Contains(wsNonLTS[0].Message, "LTS") {
		t.Fatalf("non-LTS target must warn: %+v", wsNonLTS)
	}

	// 非 LTS 間でマイナーを飛ばすケース (26.4 -> 26.8 は 26.5..26.7 を飛ばす)
	wsMinorSkip := upgradePathWarnings("26.4.1.1", "26.8.1.1")
	if len(wsMinorSkip) == 0 || wsMinorSkip[0].Level != "warning" {
		t.Fatalf("skipping minor releases from non-LTS must warn: %+v", wsMinorSkip)
	}

	// 月が 1..2 や 9..12 のケースでも LTS 飛び越しと非 LTS を正しく検出する
	wsLate := upgradePathWarnings("25.10.1.1", "26.10.1.1")
	if len(wsLate) != 2 || !strings.Contains(wsLate[0].Message, "26.3") || !strings.Contains(wsLate[0].Message, "26.8") {
		t.Fatalf("late-month LTS skip + non-LTS target: %+v", wsLate)
	}
	wsEarly := upgradePathWarnings("25.1.1.1", "25.5.1.1")
	if len(wsEarly) != 2 || !strings.Contains(wsEarly[0].Message, "25.3") {
		t.Fatalf("early-month LTS skip + non-LTS target: %+v", wsEarly)
	}

	// 不正な版・ダウングレード・月が範囲外(1.0 -> 2.0)はパニックせず警告なし
	for _, c := range [][2]string{{"bad", "26.3"}, {"26.3", "25.8"}, {"1.0", "2.0"}} {
		if ws := upgradePathWarnings(c[0], c[1]); len(ws) != 0 {
			t.Errorf("%s -> %s: expected no warnings, got %+v", c[0], c[1], ws)
		}
	}
}

func TestParseCounts_JSONCompactから件数を読み取り不正な応答は拒否する(t *testing.T) {
	got, err := parseCounts([]byte(`{"meta":[{"name":"c","type":"UInt64"}],"data":[["0"]],"rows":1}`), 1)
	if err != nil || len(got) != 1 || got[0] != 0 {
		t.Fatalf("string UInt64: %v %v", got, err)
	}
	got, err = parseCounts([]byte(`{"meta":[{"name":"t"},{"name":"p"}],"data":[[3,"12"]],"rows":1}`), 2)
	if err != nil || len(got) != 2 || got[0] != 3 || got[1] != 12 {
		t.Fatalf("two counts: %v %v", got, err)
	}
	for _, bad := range []string{
		`not json`,
		`{"meta":[{"name":"c"}],"data":[],"rows":0}`,
		`{"meta":[{"name":"c"}],"data":[[null]],"rows":1}`,
		`{"meta":[{"name":"c"}],"data":[["abc"]],"rows":1}`,
		`{"meta":[{"name":"c"}],"data":[["-1"]],"rows":1}`,
	} {
		if _, err := parseCounts([]byte(bad), 1); err == nil {
			t.Errorf("expected error for %s", bad)
		}
	}
}

func TestFreezeToken_凍結状態を往復でき空や壊れたトークンはfalseを返す(t *testing.T) {
	want := freezeState{Host: "127.0.0.1", Port: 18123, StoppedMerges: true, StoppedMoves: true}
	tok := encodeFreezeToken(want)
	got, ok := decodeFreezeToken(tok)
	if !ok || got != want {
		t.Fatalf("roundtrip = %+v (ok=%v), want %+v", got, ok, want)
	}
	if _, ok := decodeFreezeToken(""); ok {
		t.Fatal("empty token must decode as ok=false")
	}
	if _, ok := decodeFreezeToken("{broken"); ok {
		t.Fatal("corrupt token must decode as ok=false")
	}
}
