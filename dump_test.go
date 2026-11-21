package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestValidateIdent_安全な識別子だけを許可しバッククォートやバックスラッシュや制御文字を拒否する(t *testing.T) {
	for _, ok := range []string{"app", "events_2026", "my-db", "table.v1", "A1$b"} {
		q, err := quoteIdent(ok)
		if err != nil {
			t.Errorf("quoteIdent(%q): unexpected error: %v", ok, err)
		}
		if q != "`"+ok+"`" {
			t.Errorf("quoteIdent(%q) = %q, want %q", ok, q, "`"+ok+"`")
		}
	}

	for _, bad := range []string{
		"",
		"bad`name",
		`bad\name`,
		"bad\nname",
		"bad\rname",
		"bad\x00name",
		"bad\x7fname",
		strings.Repeat("a", 257),
	} {
		if _, err := quoteIdent(bad); err == nil {
			t.Errorf("quoteIdent(%q): expected error, got nil", bad)
		}
	}
}

func TestIsForbiddenDatabase_システム系データベースは大文字小文字を問わず拒否する(t *testing.T) {
	for _, forbidden := range []string{
		"system", "SYSTEM", "System",
		"INFORMATION_SCHEMA", "information_schema", "Information_Schema",
	} {
		if !isForbiddenDatabase(forbidden) {
			t.Errorf("%q must be forbidden", forbidden)
		}
	}
	for _, ok := range []string{"default", "app", "analytics", "system_logs"} {
		if isForbiddenDatabase(ok) {
			t.Errorf("%q must be allowed", ok)
		}
	}
}

func TestIsAllowedTableEngine_安全なローカルエンジンだけ許可し外部資源エンジンは拒否する(t *testing.T) {
	allowed := []string{
		"MergeTree",
		"ReplacingMergeTree",
		"SummingMergeTree",
		"AggregatingMergeTree",
		"CollapsingMergeTree",
		"VersionedCollapsingMergeTree",
		"GraphiteMergeTree",
		"CoalescingMergeTree",
		"Log",
		"TinyLog",
		"StripeLog",
		"Memory",
		"Null",
		"View",
		"MaterializedView",
	}
	for _, e := range allowed {
		if !isAllowedTableEngine(e) {
			t.Errorf("engine %q must be allowed", e)
		}
	}

	rejected := []string{
		"",
		"URL",
		"S3",
		"S3Queue",
		"GCS",
		"AzureBlobStorage",
		"HDFS",
		"File",
		"FileLog",
		"MySQL",
		"MaterializedMySQL",
		"PostgreSQL",
		"MaterializedPostgreSQL",
		"MongoDB",
		"Kafka",
		"RabbitMQ",
		"NATS",
		"JDBC",
		"ODBC",
		"Redis",
		"SQLite",
		"Distributed",
		"ExternalDistributed",
		"Executable",
		"ExecutablePool",
		"Dictionary",
		"ReplicatedMergeTree",
		"ReplicatedReplacingMergeTree",
		"SharedMergeTree",
		"Iceberg",
		"DeltaLake",
		"Hudi",
		"Hive",
		"Merge",
		"Buffer",
		"LiveView",
		"WindowView",
	}
	for _, e := range rejected {
		if isAllowedTableEngine(e) {
			t.Errorf("engine %q must be rejected", e)
		}
	}
}

func TestValidateTableDDL_許可エンジンと安全なDDLを通し危険なエンジンや関数や改ざんを拒否する(t *testing.T) {
	validCases := []struct {
		db, table, engine, ddl string
	}{
		{
			db: "app", table: "events", engine: "MergeTree",
			ddl: "CREATE TABLE app.events (`id` UInt64, `url` String, `file` Nullable(String), `s3` String) ENGINE = MergeTree ORDER BY (id, url) SETTINGS index_granularity = 8192",
		},
		{
			db: "app", table: "dedup", engine: "ReplacingMergeTree",
			ddl: "CREATE TABLE `app`.`dedup` (`id` UInt64, `ver` UInt32) ENGINE = ReplacingMergeTree(ver) ORDER BY id",
		},
		{
			db: "app", table: "mem", engine: "Memory",
			ddl: "CREATE TABLE app.mem (`k` String, `v` Int64) ENGINE = Memory",
		},
		{
			db: "app", table: "sink", engine: "Null",
			ddl: "CREATE TABLE app.sink (`k` String) ENGINE = Null",
		},
		{
			db: "app", table: "tlog", engine: "TinyLog",
			ddl: "CREATE TABLE app.tlog (`x` UInt8) ENGINE = TinyLog",
		},
		{
			db: "app", table: "v_events", engine: "View",
			ddl: "CREATE VIEW app.v_events (`id` UInt64, `note` String) AS SELECT id, 'url(http://example.com)' AS note FROM app.events",
		},
		{
			db: "app", table: "mv_to", engine: "MaterializedView",
			ddl: "CREATE MATERIALIZED VIEW app.mv_to TO app.dedup (`id` UInt64, `ver` UInt32) AS SELECT id, 1 AS ver FROM app.events",
		},
		{
			db: "app", table: "mv_inner", engine: "MaterializedView",
			ddl: "CREATE MATERIALIZED VIEW app.mv_inner (`id` UInt64) ENGINE = MergeTree ORDER BY id AS SELECT id FROM app.events",
		},
	}
	for _, tc := range validCases {
		if err := validateTableDDL(tc.db, tc.table, tc.engine, tc.ddl); err != nil {
			t.Errorf("%s.%s (%s): unexpected error: %v", tc.db, tc.table, tc.engine, err)
		}
	}

	invalidCases := []struct {
		name                   string
		db, table, engine, ddl string
		wantSubstr             string
	}{
		{
			name: "DDLのENGINE句がURL",
			db:   "app", table: "ext", engine: "MergeTree",
			ddl:        "CREATE TABLE app.ext (`x` String) ENGINE = URL('http://169.254.169.254/latest/meta-data/', 'LineAsString')",
			wantSubstr: "URL",
		},
		{
			name: "DDLのENGINE句がS3",
			db:   "app", table: "ext", engine: "MergeTree",
			ddl:        "CREATE TABLE app.ext (`x` String) ENGINE = S3('https://bucket/key', 'Parquet')",
			wantSubstr: "S3",
		},
		{
			name: "MaterializedViewの内部ENGINEがFile",
			db:   "app", table: "mv_bad", engine: "MaterializedView",
			ddl:        "CREATE MATERIALIZED VIEW app.mv_bad (`x` String) ENGINE = File(TSV) AS SELECT 'a' AS x",
			wantSubstr: "File",
		},
		{
			name: "VIEWがurl関数を大文字で呼ぶ",
			db:   "app", table: "v_url", engine: "View",
			ddl:        "CREATE VIEW app.v_url AS SELECT * FROM URL('http://127.0.0.1:8123/', 'LineAsString')",
			wantSubstr: "url",
		},
		{
			name: "VIEWがコメントと空白を挟んでs3関数を呼ぶ",
			db:   "app", table: "v_s3", engine: "View",
			ddl:        "CREATE VIEW app.v_s3 AS SELECT * FROM s3 /* sneaky */ ('https://example.com/a', 'TSV')",
			wantSubstr: "s3",
		},
		{
			name: "VIEWがfile関数をバッククォート付きで呼ぶ",
			db:   "app", table: "v_file", engine: "View",
			ddl:        "CREATE VIEW app.v_file AS SELECT * FROM `file`('/etc/passwd', 'LineAsString')",
			wantSubstr: "file",
		},
		{
			name: "VIEWがmysql関数を呼ぶ",
			db:   "app", table: "v_mysql", engine: "View",
			ddl:        "CREATE VIEW app.v_mysql AS SELECT * FROM MySQL('10.0.0.1:3306', 'db', 't', 'u', 'p')",
			wantSubstr: "mysql",
		},
		{
			name: "VIEWがremote関数を呼ぶ",
			db:   "app", table: "v_remote", engine: "View",
			ddl:        "CREATE VIEW app.v_remote AS SELECT * FROM remote('10.0.0.2', 'db', 't')",
			wantSubstr: "remote",
		},
		{
			name: "VIEWがexecutable関数を呼ぶ",
			db:   "app", table: "v_exec", engine: "View",
			ddl:        "CREATE VIEW app.v_exec AS SELECT * FROM executable('cat /etc/passwd', 'TabSeparated', 's String')",
			wantSubstr: "executable",
		},
		{
			name: "列のDEFAULT式にdictGetを含む",
			db:   "app", table: "t_dict", engine: "MergeTree",
			ddl:        "CREATE TABLE app.t_dict (`id` UInt64, `v` String DEFAULT dictGet('ext_dict', 'v', id)) ENGINE = MergeTree ORDER BY id",
			wantSubstr: "dictget",
		},
		{
			name: "複数文の注入",
			db:   "app", table: "events", engine: "MergeTree",
			ddl:        "CREATE TABLE app.events (`id` UInt64) ENGINE = MergeTree ORDER BY id; CREATE USER evil IDENTIFIED BY 'x'",
			wantSubstr: "single statement",
		},
		{
			name: "CREATE TABLE以外の文",
			db:   "app", table: "events", engine: "MergeTree",
			ddl:        "SYSTEM RELOAD CONFIG",
			wantSubstr: "unsupported DDL",
		},
		{
			name: "DDLの対象テーブルがメタデータと不一致",
			db:   "app", table: "events", engine: "MergeTree",
			ddl:        "CREATE TABLE other.events (`id` UInt64) ENGINE = MergeTree ORDER BY id",
			wantSubstr: "does not match",
		},
		{
			name: "ENGINE句が無いCREATE TABLE",
			db:   "app", table: "events", engine: "MergeTree",
			ddl:        "CREATE TABLE app.events (`id` UInt64)",
			wantSubstr: "ENGINE",
		},
		{
			name: "MaterializedViewが別DBのsystemを参照しないようTO先チェックは不要だがVIEWにENGINE句があれば拒否",
			db:   "app", table: "v_bad", engine: "View",
			ddl:        "CREATE TABLE app.v_bad (`id` UInt64) ENGINE = MergeTree ORDER BY id",
			wantSubstr: "View",
		},
	}
	for _, tc := range invalidCases {
		err := validateTableDDL(tc.db, tc.table, tc.engine, tc.ddl)
		if err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantSubstr)) {
			t.Errorf("%s: err = %q, want substring %q", tc.name, err.Error(), tc.wantSubstr)
		}
	}
}

func TestMaterializedViewHasTargetTO_TO句の有無を正しく判定する(t *testing.T) {
	if !materializedViewHasTargetTO("CREATE MATERIALIZED VIEW app.mv TO app.dst (`x` UInt32) AS SELECT x FROM app.src") {
		t.Error("expected true for MV with TO clause")
	}
	if !materializedViewHasTargetTO("CREATE MATERIALIZED VIEW `app`.`mv` TO `app`.`dst` AS SELECT x FROM app.src") {
		t.Error("expected true for quoted MV with TO clause")
	}
	if materializedViewHasTargetTO("CREATE MATERIALIZED VIEW app.mv (`to` UInt32) ENGINE = MergeTree ORDER BY to AS SELECT 1 AS to") {
		t.Error("expected false for MV with inner ENGINE even if column is named 'to'")
	}
}

func TestDumpStream_ヘッダと複数テーブルのDDLとNativeブロックを往復でき改ざんや上限超過を拒否する(t *testing.T) {
	hdr := dumpHeader{
		Format:            dumpFormatMagic,
		Version:           dumpFormatVersion,
		ClickHouseVersion: "26.3.19.3",
		CreatedAt:         "2026-11-21T09:00:00Z",
		Databases:         []string{"app"},
		Tables: []dumpTableMeta{
			{Database: "app", Name: "events", Engine: "MergeTree", HasData: true},
			{Database: "app", Name: "empty", Engine: "Memory", HasData: true},
			{Database: "app", Name: "v_events", Engine: "View", HasData: false},
		},
	}

	var buf bytes.Buffer
	w := newDumpWriter(&buf, 1<<20)
	if err := w.WriteHeader(hdr); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	// Table 1: large payload split across multiple chunks
	payload1 := bytes.Repeat([]byte{0x01, 0x02, 0x03, 0xfe, 0xff}, 4000)
	ddl1 := "CREATE TABLE app.events (`id` UInt64) ENGINE = MergeTree ORDER BY id"
	if err := w.WriteTable(hdr.Tables[0], ddl1, bytes.NewReader(payload1), 1024); err != nil {
		t.Fatalf("WriteTable 1: %v", err)
	}
	// Table 2: empty data stream
	ddl2 := "CREATE TABLE app.empty (`id` UInt64) ENGINE = Memory"
	if err := w.WriteTable(hdr.Tables[1], ddl2, bytes.NewReader(nil), 1024); err != nil {
		t.Fatalf("WriteTable 2: %v", err)
	}
	// Table 3: view (no data)
	ddl3 := "CREATE VIEW app.v_events AS SELECT id FROM app.events"
	if err := w.WriteTable(hdr.Tables[2], ddl3, nil, 1024); err != nil {
		t.Fatalf("WriteTable 3: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Read back
	r := newDumpReader(bytes.NewReader(buf.Bytes()), defaultMaxDumpTables)
	gotHdr, err := r.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if gotHdr.Version != 1 || gotHdr.ClickHouseVersion != "26.3.19.3" || len(gotHdr.Tables) != 3 {
		t.Fatalf("header = %+v", gotHdr)
	}

	rec1, gotDDL1, data1, err := r.NextTable()
	if err != nil {
		t.Fatalf("NextTable 1: %v", err)
	}
	if rec1 != hdr.Tables[0] || gotDDL1 != ddl1 {
		t.Fatalf("table 1 mismatch: %+v %q", rec1, gotDDL1)
	}
	gotPayload1, err := io.ReadAll(data1)
	if err != nil || !bytes.Equal(gotPayload1, payload1) {
		t.Fatalf("payload 1 len=%d err=%v", len(gotPayload1), err)
	}

	rec2, gotDDL2, data2, err := r.NextTable()
	if err != nil {
		t.Fatalf("NextTable 2: %v", err)
	}
	if rec2 != hdr.Tables[1] || gotDDL2 != ddl2 {
		t.Fatalf("table 2 mismatch: %+v %q", rec2, gotDDL2)
	}
	gotPayload2, err := io.ReadAll(data2)
	if err != nil || len(gotPayload2) != 0 {
		t.Fatalf("payload 2 len=%d err=%v", len(gotPayload2), err)
	}

	rec3, gotDDL3, data3, err := r.NextTable()
	if err != nil {
		t.Fatalf("NextTable 3: %v", err)
	}
	if rec3 != hdr.Tables[2] || gotDDL3 != ddl3 || data3 != nil {
		t.Fatalf("table 3 mismatch: %+v %q data=%v", rec3, gotDDL3, data3)
	}

	if err := r.VerifyEOF(); err != nil {
		t.Fatalf("VerifyEOF: %v", err)
	}
}

func TestDumpReader_未知の版や途中切断や末尾ゴミや上限超過を拒否する(t *testing.T) {
	// Unknown version
	badVer := `{"format":"db-branch-clickhouse-dump","version":99,"clickhouse_version":"26.3","created_at":"2026-11-21T00:00:00Z","databases":["app"],"tables":[]}` + "\n"
	if _, err := newDumpReader(strings.NewReader(badVer), 100).ReadHeader(); err == nil {
		t.Fatal("unknown format version must be rejected")
	}

	// Wrong format magic
	badFmt := `{"format":"other","version":1,"databases":["app"],"tables":[]}` + "\n"
	if _, err := newDumpReader(strings.NewReader(badFmt), 100).ReadHeader(); err == nil {
		t.Fatal("unknown format magic must be rejected")
	}

	// Forbidden DB in header
	badDB := `{"format":"db-branch-clickhouse-dump","version":1,"databases":["system"],"tables":[]}` + "\n"
	if _, err := newDumpReader(strings.NewReader(badDB), 100).ReadHeader(); err == nil {
		t.Fatal("forbidden database in header must be rejected")
	}

	// Too many tables in header
	tooMany := `{"format":"db-branch-clickhouse-dump","version":1,"databases":["app"],"tables":[{"database":"app","name":"t1","engine":"Memory"},{"database":"app","name":"t2","engine":"Memory"}]}` + "\n"
	if _, err := newDumpReader(strings.NewReader(tooMany), 1).ReadHeader(); err == nil {
		t.Fatal("tables exceeding limit must be rejected")
	}

	// Size limit exceeded during write
	var small bytes.Buffer
	w := newDumpWriter(&small, 200)
	hdr := dumpHeader{
		Format: dumpFormatMagic, Version: dumpFormatVersion,
		Databases: []string{"app"},
		Tables:    []dumpTableMeta{{Database: "app", Name: "t", Engine: "Memory", HasData: true}},
	}
	if err := w.WriteHeader(hdr); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	err := w.WriteTable(hdr.Tables[0], "CREATE TABLE app.t (`x` UInt8) ENGINE = Memory", bytes.NewReader(bytes.Repeat([]byte("a"), 500)), 64)
	if !errors.Is(err, errDumpSizeLimitExceeded) {
		t.Fatalf("expected errDumpSizeLimitExceeded, got %v", err)
	}

	// Truncated block stream
	var truncBuf bytes.Buffer
	w2 := newDumpWriter(&truncBuf, 1<<20)
	_ = w2.WriteHeader(hdr)
	_ = w2.WriteTable(hdr.Tables[0], "CREATE TABLE app.t (`x` UInt8) ENGINE = Memory", bytes.NewReader([]byte("hello world")), 64)
	_ = w2.Flush()
	raw := truncBuf.Bytes()
	r := newDumpReader(bytes.NewReader(raw[:len(raw)-6]), 100) // cut off before sentinel
	if _, err := r.ReadHeader(); err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	_, _, data, err := r.NextTable()
	if err != nil {
		t.Fatalf("NextTable: %v", err)
	}
	if _, err := io.ReadAll(data); err == nil {
		t.Fatal("truncated block stream must return an error")
	}

	// Trailing garbage
	r2 := newDumpReader(bytes.NewReader(append(append([]byte(nil), raw...), "extra"...)), 100)
	if _, err := r2.ReadHeader(); err != nil {
		t.Fatal(err)
	}
	_, _, data, err = r2.NextTable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(data); err != nil {
		t.Fatal(err)
	}
	if err := r2.VerifyEOF(); err == nil {
		t.Fatal("trailing garbage must be rejected")
	}
}

func TestShortClickHouseErrorとScrubSecret_長いスタックトレースを縮め秘密情報を伏せる(t *testing.T) {
	body := []byte("Code: 62. DB::Exception: Syntax error: failed at position 1. (SYNTAX_ERROR) (version 26.3.19.3)\nStack trace:\n0. something\n")
	got := shortClickHouseError(body)
	if !strings.Contains(got, "Code: 62.") || strings.Contains(got, "Stack trace:") {
		t.Fatalf("shortClickHouseError = %q", got)
	}
	if empty := shortClickHouseError([]byte("  \n ")); empty != "empty error response" {
		t.Fatalf("empty error = %q", empty)
	}
	if long := shortClickHouseError([]byte(strings.Repeat("x", 400))); len(long) != 303 || !strings.HasSuffix(long, "...") {
		t.Fatalf("long error len=%d", len(long))
	}
	if s := scrubSecret("auth failed for secret-pw-123 here", "secret-pw-123"); strings.Contains(s, "secret-pw-123") || !strings.Contains(s, "***") {
		t.Fatalf("scrubSecret = %q", s)
	}
	if s := scrubSecret("plain", ""); s != "plain" {
		t.Fatalf("scrubSecret empty = %q", s)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

type failAfterWriter struct {
	limit int
	wrote int
}

func (f *failAfterWriter) Write(p []byte) (int, error) {
	if f.wrote+len(p) > f.limit {
		return 0, errors.New("disk full")
	}
	f.wrote += len(p)
	return len(p), nil
}

func TestDumpEdgeCases_トークナイザやDDL検証やブロック読み書きの異常系を網羅する(t *testing.T) {
	// Tokenizer: line comments, block comments, escaped quotes, double-quoted idents, and unterminated literals.
	toks, err := tokenizeSQL("SELECT -- line comment\n/* block */ \"a\"\"b\", `c\\`d`, 'e\\'f''g'")
	if err != nil || len(toks) < 5 {
		t.Fatalf("tokenizeSQL with comments/escapes: %v %v", toks, err)
	}
	validWithComments := "CREATE TABLE \"app\".`events` (\n-- comment\n`id` UInt64, `msg` String DEFAULT 'it\\'s ''ok''' /* block */) ENGINE = MergeTree ORDER BY id"
	if err := validateTableDDL("app", "events", "MergeTree", validWithComments); err != nil {
		t.Fatalf("valid DDL with comments/escapes failed: %v", err)
	}
	for _, bad := range []string{
		"CREATE TABLE app.t (`x` String) /* unterminated",
		"CREATE TABLE app.t (`x` String DEFAULT 'unterminated) ENGINE = Memory",
		"CREATE TABLE app.`unterminated (`x` String) ENGINE = Memory",
	} {
		if _, err := tokenizeSQL(bad); err == nil {
			t.Errorf("tokenizeSQL(%q): expected error", bad)
		}
		if err := validateTableDDL("app", "t", "Memory", bad); err == nil {
			t.Errorf("validateTableDDL(%q): expected error", bad)
		}
	}

	// validateTableDDL edge cases
	badDDLs := []struct{ db, tbl, eng, ddl string }{
		{"bad`db", "t", "Memory", "CREATE TABLE bad`db.t (`x` UInt8) ENGINE = Memory"},
		{"app", "bad`t", "Memory", "CREATE TABLE app.bad`t (`x` UInt8) ENGINE = Memory"},
		{"system", "t", "Memory", "CREATE TABLE system.t (`x` UInt8) ENGINE = Memory"},
		{"app", "t", "URL", "CREATE TABLE app.t (`x` UInt8) ENGINE = URL('http://x')"},
		{"app", "t", "Memory", ""},
		{"app", "t", "Memory", "CREATE VIEW app.t AS SELECT 1"},
		{"app", "mv", "MaterializedView", "CREATE TABLE app.mv (`x` UInt8) ENGINE = Memory"},
		{"app", "t", "Memory", "CREATE TABLE t (`x` UInt8) ENGINE = Memory"},
		{"app", "t", "Memory", "CREATE TABLE app.t (`x` UInt8) ENGINE ="},
		{"app", "v", "View", "CREATE VIEW app.v ENGINE = Memory AS SELECT 1"},
		{"app", "mv", "MaterializedView", "CREATE MATERIALIZED VIEW app.mv AS SELECT 1"},
	}
	for _, tc := range badDDLs {
		if err := validateTableDDL(tc.db, tc.tbl, tc.eng, tc.ddl); err == nil {
			t.Errorf("expected error for %+v", tc)
		}
	}

	// tableHasData & materializedViewHasTargetTO edge cases
	if tableHasData("Null", "CREATE TABLE app.n (`x` UInt8) ENGINE = Null") {
		t.Error("Null engine must have HasData=false")
	}
	if materializedViewHasTargetTO("short") {
		t.Error("short DDL must return false")
	}
	if materializedViewHasTargetTO("CREATE MATERIALIZED VIEW app.mv (`x` UInt8)") {
		t.Error("MV without TO/ENGINE/AS must return false")
	}

	// dumpWriter default/clamped chunkSize and error propagation at header, record, DDL, block header
	var okBuf bytes.Buffer
	w := newDumpWriter(&okBuf, 0)
	if err := w.WriteTable(dumpTableMeta{Database: "app", Name: "t", Engine: "Memory", HasData: true}, "CREATE TABLE app.t (`x` UInt8) ENGINE = Memory", bytes.NewReader([]byte("abc")), 0); err != nil {
		t.Fatalf("default chunk size: %v", err)
	}
	if err := w.WriteTable(dumpTableMeta{Database: "app", Name: "t", Engine: "Memory", HasData: true}, "CREATE TABLE app.t (`x` UInt8) ENGINE = Memory", errReader{err: errors.New("read boom")}, maxDumpChunkBytes+1); err == nil {
		t.Fatal("data reader error must propagate")
	}
	// Limit hit during header, table record, DDL, and block length header
	if err := newDumpWriter(io.Discard, 5).WriteHeader(dumpHeader{Format: dumpFormatMagic, Version: 1, Databases: []string{"app"}}); !errors.Is(err, errDumpSizeLimitExceeded) {
		t.Fatalf("header size limit: %v", err)
	}
	if err := newDumpWriter(io.Discard, 10).WriteTable(dumpTableMeta{Database: "app", Name: "t", Engine: "Memory"}, "CREATE TABLE app.t (`x` UInt8) ENGINE = Memory", nil, 64); !errors.Is(err, errDumpSizeLimitExceeded) {
		t.Fatalf("record size limit: %v", err)
	}
	if err := newDumpWriter(io.Discard, 75).WriteTable(dumpTableMeta{Database: "app", Name: "t", Engine: "Memory"}, "CREATE TABLE app.t (`x` UInt8) ENGINE = Memory", nil, 64); !errors.Is(err, errDumpSizeLimitExceeded) {
		t.Fatalf("DDL size limit: %v", err)
	}
	if err := newDumpWriter(io.Discard, 120).WriteTable(dumpTableMeta{Database: "app", Name: "t", Engine: "Memory", HasData: true}, "CREATE TABLE app.t (`x` UInt8) ENGINE = Memory", bytes.NewReader([]byte("x")), 64); !errors.Is(err, errDumpSizeLimitExceeded) {
		t.Fatalf("block header size limit: %v", err)
	}
	wFail := newDumpWriter(&failAfterWriter{limit: 4}, 0)
	_ = wFail.WriteHeader(dumpHeader{Format: dumpFormatMagic, Version: 1, Databases: []string{"app"}})
	if err := wFail.Flush(); err == nil {
		t.Fatal("flush to failing writer must fail")
	}

	// dumpReader error branches: NextTable before header, NextTable past end (io.EOF), bad headers, missing sentinel EOF
	rUninit := newDumpReader(strings.NewReader(""), 0)
	if _, _, _, err := rUninit.NextTable(); err == nil {
		t.Fatal("NextTable before ReadHeader must fail")
	}
	if err := rUninit.VerifyEOF(); err == nil {
		t.Fatal("VerifyEOF before ReadHeader must fail")
	}
	rEmptyTables := newDumpReader(strings.NewReader(`{"format":"db-branch-clickhouse-dump","version":1,"databases":["app"],"tables":[]}`+"\n"), 10)
	if _, err := rEmptyTables.ReadHeader(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := rEmptyTables.NextTable(); !errors.Is(err, io.EOF) {
		t.Fatalf("NextTable past end = %v, want io.EOF", err)
	}
	// Missing sentinel right at EOF
	missingSentinel := &blockStreamReader{r: bytes.NewReader(nil)}
	if _, err := missingSentinel.HasData(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("missing sentinel = %v, want ErrUnexpectedEOF", err)
	}
	// Read hitting immediate 0 sentinel without prior HasData
	zeroDirect := &blockStreamReader{r: bytes.NewReader([]byte{0, 0, 0, 0})}
	if n, err := zeroDirect.Read(make([]byte, 4)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("zeroDirect.Read = (%d, %v)", n, err)
	}
	for _, badHdr := range []string{
		"",
		"not json\n",
		`{"format":"db-branch-clickhouse-dump","version":1,"databases":[]}` + "\n",
		`{"format":"db-branch-clickhouse-dump","version":1,"databases":["bad` + "`" + `db"]}` + "\n",
		`{"format":"db-branch-clickhouse-dump","version":1,"databases":["app","app"]}` + "\n",
		`{"format":"db-branch-clickhouse-dump","version":1,"databases":["app"],"tables":[{"database":"other","name":"t","engine":"Memory"}]}` + "\n",
		`{"format":"db-branch-clickhouse-dump","version":1,"databases":["app"],"tables":[{"database":"app","name":"bad` + "`" + `t","engine":"Memory"}]}` + "\n",
		`{"format":"db-branch-clickhouse-dump","version":1,"databases":["app"],"tables":[{"database":"app","name":"t","engine":"URL"}]}` + "\n",
	} {
		if _, err := newDumpReader(strings.NewReader(badHdr), 10).ReadHeader(); err == nil {
			t.Errorf("ReadHeader(%q): expected error", badHdr)
		}
	}

	// Oversized line check
	if _, err := readBoundedLine(newDumpReader(strings.NewReader(strings.Repeat("a", 20)+"\n"), 10).br, 10); err == nil {
		t.Fatal("oversized line must fail")
	}

	// Mismatched table record & invalid DDL length & EOF on NextTable
	hdrLine := `{"format":"db-branch-clickhouse-dump","version":1,"databases":["app"],"tables":[{"database":"app","name":"t","engine":"Memory","has_data":true}]}` + "\n"
	for _, rec := range []string{
		"",
		"not json\n",
		`{"database":"app","name":"other","engine":"Memory","has_data":true,"ddl_len":5}` + "\n12345",
		`{"database":"app","name":"t","engine":"Memory","has_data":true,"ddl_len":0}` + "\n",
		`{"database":"app","name":"t","engine":"Memory","has_data":true,"ddl_len":50}` + "\nshort",
	} {
		r := newDumpReader(strings.NewReader(hdrLine+rec), 10)
		if _, err := r.ReadHeader(); err != nil {
			t.Fatalf("ReadHeader: %v", err)
		}
		if _, _, _, err := r.NextTable(); err == nil {
			t.Errorf("NextTable(%q): expected error", rec)
		}
	}

	// Oversized chunk length in blockStreamReader + HasData() idempotence + empty block stream
	var bigChunk bytes.Buffer
	bigChunk.Write([]byte{0x7f, 0x00, 0x00, 0x00}) // > maxDumpChunkBytes
	bs := &blockStreamReader{r: &bigChunk}
	if _, err := bs.HasData(); err == nil {
		t.Fatal("oversized chunk must fail")
	}
	if _, err := bs.HasData(); err == nil {
		t.Fatal("sticky error on HasData")
	}
	if _, err := bs.Read(make([]byte, 8)); err == nil {
		t.Fatal("sticky error on Read")
	}

	emptyBS := &blockStreamReader{r: bytes.NewReader([]byte{0, 0, 0, 0})}
	has, err := emptyBS.HasData()
	if err != nil || has {
		t.Fatalf("empty HasData = (%v, %v)", has, err)
	}
	if has, err := emptyBS.HasData(); err != nil || has || !emptyBS.Done() {
		t.Fatalf("repeated empty HasData = (%v, %v, done=%v)", has, err, emptyBS.Done())
	}
	if _, err := emptyBS.Read(make([]byte, 4)); !errors.Is(err, io.EOF) {
		t.Fatalf("read after done = %v", err)
	}

	// Non-empty HasData called twice before Read
	oneBS := &blockStreamReader{r: bytes.NewReader([]byte{0, 0, 0, 2, 'h', 'i', 0, 0, 0, 0})}
	if has, err := oneBS.HasData(); err != nil || !has {
		t.Fatalf("HasData #1 = (%v, %v)", has, err)
	}
	if has, err := oneBS.HasData(); err != nil || !has {
		t.Fatalf("HasData #2 = (%v, %v)", has, err)
	}
	gotData, err := io.ReadAll(oneBS)
	if err != nil || string(gotData) != "hi" || !oneBS.Done() || oneBS.Err() != nil {
		t.Fatalf("ReadAll = %q err=%v done=%v", gotData, err, oneBS.Done())
	}

	// Truncated inside chunk payload (header says 10 bytes, only 2 present)
	truncBody := &blockStreamReader{r: bytes.NewReader([]byte{0, 0, 0, 10, 'a', 'b'})}
	if _, err := io.ReadAll(truncBody); err == nil {
		t.Fatal("truncated chunk body must error")
	}
}
