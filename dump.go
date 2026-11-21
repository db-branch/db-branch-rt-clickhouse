package main

import (
	"bufio"
	encbin "encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	dumpFormatMagic      = "db-branch-clickhouse-dump"
	dumpFormatVersion    = 1
	defaultMaxDumpBytes  = int64(64) << 30 // 64 GiB
	defaultMaxDumpTables = 10000
	defaultDumpChunkSize = 256 << 10 // 256 KiB

	maxDumpHeaderBytes = 16 << 20 // 16 MiB
	maxDumpDDLBytes    = 4 << 20  // 4 MiB
	maxDumpChunkBytes  = 16 << 20 // 16 MiB
)

var errDumpSizeLimitExceeded = errors.New("dump size exceeds maximum allowed bytes")

// validateIdent ensures a ClickHouse database or table identifier is safe to
// quote with backticks and splice into SQL statements.
func validateIdent(name string) error {
	if name == "" {
		return errors.New("identifier is empty")
	}
	if len(name) > 256 {
		return fmt.Errorf("identifier %q is too long (%d bytes)", name[:32], len(name))
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '`' || c == '\\' || c < 0x20 || c == 0x7f {
			return fmt.Errorf("identifier %q contains disallowed character", name)
		}
	}
	return nil
}

func quoteIdent(name string) (string, error) {
	if err := validateIdent(name); err != nil {
		return "", err
	}
	return "`" + name + "`", nil
}

func quoteSQLString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `''`)
	return "'" + s + "'"
}

func isForbiddenDatabase(name string) bool {
	return strings.EqualFold(name, "system") || strings.EqualFold(name, "information_schema")
}

var allowedDatabaseEngines = map[string]bool{
	"Atomic":   true,
	"Ordinary": true,
	"Memory":   true,
	"Lazy":     true,
}

func isAllowedDatabaseEngine(engine string) bool {
	return allowedDatabaseEngines[engine]
}

// allowedDataEngines is the strict allowlist of local ClickHouse storage
// engines permitted in dumps. Replicated/Shared engines (which require
// Keeper/ZooKeeper) and all external-resource engines are excluded.
var allowedDataEngines = map[string]bool{
	"MergeTree":                    true,
	"ReplacingMergeTree":           true,
	"SummingMergeTree":             true,
	"AggregatingMergeTree":         true,
	"CollapsingMergeTree":          true,
	"VersionedCollapsingMergeTree": true,
	"GraphiteMergeTree":            true,
	"CoalescingMergeTree":          true,
	"Log":                          true,
	"TinyLog":                      true,
	"StripeLog":                    true,
	"Memory":                       true,
	"Null":                         true,
}

func isAllowedTableEngine(engine string) bool {
	return allowedDataEngines[engine] || engine == "View" || engine == "MaterializedView"
}

var dangerousFunctions = map[string]bool{
	"url":                     true,
	"urlcluster":              true,
	"s3":                      true,
	"s3cluster":               true,
	"gcs":                     true,
	"cosn":                    true,
	"oss":                     true,
	"azureblobstorage":        true,
	"azureblobstoragecluster": true,
	"hdfs":                    true,
	"hdfscluster":             true,
	"file":                    true,
	"filecluster":             true,
	"executable":              true,
	"mysql":                   true,
	"postgresql":              true,
	"mongodb":                 true,
	"sqlite":                  true,
	"odbc":                    true,
	"jdbc":                    true,
	"redis":                   true,
	"kafka":                   true,
	"rabbitmq":                true,
	"nats":                    true,
	"hive":                    true,
	"hivecluster":             true,
	"ytsaurus":                true,
	"meilisearch":             true,
	"arrowflight":             true,
	"datalake":                true,
	"datalakecatalog":         true,
	"deltalake":               true,
	"deltalakecluster":        true,
	"deltalakes3":             true,
	"deltalakeazure":          true,
	"deltalakelocal":          true,
	"iceberg":                 true,
	"icebergcluster":          true,
	"icebergs3":               true,
	"icebergs3cluster":        true,
	"icebergazure":            true,
	"icebergazurecluster":     true,
	"iceberghdfs":             true,
	"iceberghdfscluster":      true,
	"iceberglocal":            true,
	"iceberglocalcluster":     true,
	"hudi":                    true,
	"hudicluster":             true,
	"paimon":                  true,
	"paimoncluster":           true,
	"remote":                  true,
	"remotesecure":            true,
	"cluster":                 true,
	"clusterallreplicas":      true,
	"merge":                   true,
	"dictionary":              true,
	"dicthas":                 true,
	"dictisin":                true,
	"dictgetchildren":         true,
	"dictgetdescendants":      true,
	"dictgethierarchy":        true,
	"loop":                    true,
	"fuzzquery":               true,
}

func isDangerousFunction(name string) bool {
	lower := strings.ToLower(name)
	return dangerousFunctions[lower] || strings.HasPrefix(lower, "dictget")
}

type sqlTokKind int

const (
	tokIdent sqlTokKind = iota
	tokString
	tokPunct
)

type sqlToken struct {
	kind   sqlTokKind
	val    string
	quoted bool
}

// tokenizeSQL lexes a ClickHouse DDL statement, stripping whitespace and
// comments while keeping quoted identifiers, string literals, and punctuation
// intact for structural and function-call inspection.
func tokenizeSQL(sql string) ([]sqlToken, error) {
	var toks []sqlToken
	n := len(sql)
	for i := 0; i < n; {
		c := sql[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' {
			i++
			continue
		}
		if c == '-' && i+1 < n && sql[i+1] == '-' {
			for i < n && sql[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < n && sql[i+1] == '*' {
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return nil, errors.New("unterminated block comment in DDL")
			}
			i += end + 4
			continue
		}
		if c == '\'' {
			j := i + 1
			for j < n {
				if sql[j] == '\\' && j+1 < n {
					j += 2
					continue
				}
				if sql[j] == '\'' {
					if j+1 < n && sql[j+1] == '\'' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			if j >= n {
				return nil, errors.New("unterminated string literal in DDL")
			}
			toks = append(toks, sqlToken{kind: tokString, val: sql[i+1 : j]})
			i = j + 1
			continue
		}
		if c == '`' || c == '"' {
			quote := c
			j := i + 1
			var b strings.Builder
			for j < n {
				if sql[j] == '\\' && j+1 < n {
					b.WriteByte(sql[j+1])
					j += 2
					continue
				}
				if sql[j] == quote {
					if j+1 < n && sql[j+1] == quote {
						b.WriteByte(quote)
						j += 2
						continue
					}
					break
				}
				b.WriteByte(sql[j])
				j++
			}
			if j >= n {
				return nil, errors.New("unterminated quoted identifier in DDL")
			}
			toks = append(toks, sqlToken{kind: tokIdent, val: b.String(), quoted: true})
			i = j + 1
			continue
		}
		if isIdentStart(c) {
			j := i + 1
			for j < n && isIdentCont(sql[j]) {
				j++
			}
			toks = append(toks, sqlToken{kind: tokIdent, val: sql[i:j]})
			i = j
			continue
		}
		toks = append(toks, sqlToken{kind: tokPunct, val: string(c)})
		i++
	}
	return toks, nil
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isIdentCont(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func isKeyword(t sqlToken, kw string) bool {
	return t.kind == tokIdent && !t.quoted && strings.EqualFold(t.val, kw)
}

// validateTableDDL checks that ddl is a single CREATE TABLE / VIEW /
// MATERIALIZED VIEW statement targeting db.table, uses only an allowlisted
// local engine, and calls no dangerous external-access functions.
func validateTableDDL(db, table, engine, ddl string) error {
	if err := validateIdent(db); err != nil {
		return fmt.Errorf("invalid database name: %w", err)
	}
	if err := validateIdent(table); err != nil {
		return fmt.Errorf("invalid table name: %w", err)
	}
	if isForbiddenDatabase(db) {
		return fmt.Errorf("database %q is forbidden", db)
	}
	if !isAllowedTableEngine(engine) {
		return fmt.Errorf("unsupported engine %q", engine)
	}
	if len(ddl) == 0 || len(ddl) > maxDumpDDLBytes {
		return fmt.Errorf("invalid DDL size (%d bytes)", len(ddl))
	}
	stmts := splitStatements(ddl)
	if len(stmts) != 1 {
		return fmt.Errorf("DDL must be a single statement (got %d)", len(stmts))
	}
	toks, err := tokenizeSQL(stmts[0])
	if err != nil {
		return err
	}
	if len(toks) < 5 || !isKeyword(toks[0], "CREATE") {
		return errors.New("unsupported DDL: must start with CREATE TABLE/VIEW/MATERIALIZED VIEW")
	}

	var idx int
	switch engine {
	case "View":
		if !isKeyword(toks[1], "VIEW") {
			return fmt.Errorf("DDL statement does not match View engine")
		}
		idx = 2
	case "MaterializedView":
		if !isKeyword(toks[1], "MATERIALIZED") || !isKeyword(toks[2], "VIEW") {
			return fmt.Errorf("DDL statement does not match MaterializedView engine")
		}
		idx = 3
	default:
		if !isKeyword(toks[1], "TABLE") {
			return fmt.Errorf("unsupported DDL: expected CREATE TABLE for engine %s", engine)
		}
		idx = 2
	}

	// Verify target db.table matches metadata exactly.
	if idx+2 >= len(toks) || toks[idx].kind != tokIdent || toks[idx+1].kind != tokPunct || toks[idx+1].val != "." || toks[idx+2].kind != tokIdent {
		return errors.New("DDL target must be qualified as database.table")
	}
	if toks[idx].val != db || toks[idx+2].val != table {
		return fmt.Errorf("DDL target %s.%s does not match %s.%s", toks[idx].val, toks[idx+2].val, db, table)
	}
	idx += 3

	// Inspect dangerous function calls anywhere in the DDL (case-insensitive).
	for i := 0; i+1 < len(toks); i++ {
		if toks[i].kind == tokIdent && toks[i+1].kind == tokPunct && toks[i+1].val == "(" {
			if isDangerousFunction(toks[i].val) {
				return fmt.Errorf("unsupported function %q in DDL", toks[i].val)
			}
		}
	}

	// Inspect top-level ENGINE / TO clauses before AS SELECT.
	depth := 0
	foundEngine := false
	ddlEngine := ""
	foundTO := false
	for i := idx; i < len(toks); i++ {
		t := toks[i]
		if t.kind == tokPunct {
			if t.val == "(" {
				depth++
			} else if t.val == ")" && depth > 0 {
				depth--
			}
			continue
		}
		if depth != 0 {
			continue
		}
		if isKeyword(t, "AS") {
			break
		}
		if isKeyword(t, "TO") && engine == "MaterializedView" && !foundTO {
			foundTO = true
		}
		if isKeyword(t, "ENGINE") {
			next := i + 1
			if next < len(toks) && toks[next].kind == tokPunct && toks[next].val == "=" {
				next++
			}
			if next >= len(toks) || toks[next].kind != tokIdent {
				return errors.New("invalid ENGINE clause in DDL")
			}
			foundEngine = true
			ddlEngine = toks[next].val
		}
	}

	switch engine {
	case "View":
		if foundEngine {
			return fmt.Errorf("View DDL must not declare ENGINE (%s)", ddlEngine)
		}
	case "MaterializedView":
		if foundEngine {
			if !allowedDataEngines[ddlEngine] {
				return fmt.Errorf("unsupported MaterializedView inner engine %q", ddlEngine)
			}
		} else if !foundTO {
			return errors.New("MaterializedView DDL must have either TO target or an allowed ENGINE")
		}
	default:
		if !foundEngine {
			return errors.New("CREATE TABLE DDL is missing an ENGINE clause")
		}
		if !allowedDataEngines[ddlEngine] {
			return fmt.Errorf("unsupported ENGINE %q in DDL", ddlEngine)
		}
	}
	return nil
}

// materializedViewHasTargetTO reports whether a MaterializedView DDL writes to
// an explicit TO target table (and therefore has no separate inner storage).
func materializedViewHasTargetTO(ddl string) bool {
	toks, err := tokenizeSQL(ddl)
	if err != nil || len(toks) < 6 {
		return false
	}
	// Skip CREATE MATERIALIZED VIEW db . table
	depth := 0
	for i := 6; i < len(toks); i++ {
		t := toks[i]
		if t.kind == tokPunct {
			if t.val == "(" {
				depth++
			} else if t.val == ")" && depth > 0 {
				depth--
			}
			continue
		}
		if depth != 0 {
			continue
		}
		if isKeyword(t, "AS") || isKeyword(t, "ENGINE") {
			return false
		}
		if isKeyword(t, "TO") {
			return true
		}
	}
	return false
}

func tableHasData(engine, ddl string) bool {
	switch engine {
	case "View", "Null":
		return false
	case "MaterializedView":
		return !materializedViewHasTargetTO(ddl)
	default:
		return true
	}
}

type dumpHeader struct {
	Format            string          `json:"format"`
	Version           int             `json:"version"`
	ClickHouseVersion string          `json:"clickhouse_version"`
	CreatedAt         string          `json:"created_at"`
	Databases         []string        `json:"databases"`
	Tables            []dumpTableMeta `json:"tables"`
}

type dumpTableMeta struct {
	Database string `json:"database"`
	Name     string `json:"name"`
	Engine   string `json:"engine"`
	HasData  bool   `json:"has_data"`
}

type dumpTableRecord struct {
	Database string `json:"database"`
	Name     string `json:"name"`
	Engine   string `json:"engine"`
	HasData  bool   `json:"has_data"`
	DDLLen   int    `json:"ddl_len"`
}

type limitedWriter struct {
	w       io.Writer
	limit   int64
	written int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.limit > 0 && l.written+int64(len(p)) > l.limit {
		return 0, fmt.Errorf("%w (%d bytes)", errDumpSizeLimitExceeded, l.limit)
	}
	n, err := l.w.Write(p)
	l.written += int64(n)
	return n, err
}

type dumpWriter struct {
	bw  *bufio.Writer
	lim *limitedWriter
}

func newDumpWriter(w io.Writer, maxBytes int64) *dumpWriter {
	bw := bufio.NewWriterSize(w, 64<<10)
	lim := &limitedWriter{w: bw, limit: maxBytes}
	return &dumpWriter{bw: bw, lim: lim}
}

func (w *dumpWriter) WriteHeader(hdr dumpHeader) error {
	b, _ := json.Marshal(hdr)
	_, err := w.lim.Write(append(b, '\n'))
	return err
}

func (w *dumpWriter) WriteTable(meta dumpTableMeta, ddl string, data io.Reader, chunkSize int) error {
	if chunkSize <= 0 {
		chunkSize = defaultDumpChunkSize
	}
	if chunkSize > maxDumpChunkBytes {
		chunkSize = maxDumpChunkBytes
	}
	b, _ := json.Marshal(dumpTableRecord{
		Database: meta.Database,
		Name:     meta.Name,
		Engine:   meta.Engine,
		HasData:  meta.HasData,
		DDLLen:   len(ddl),
	})
	if _, err := w.lim.Write(append(b, '\n')); err != nil {
		return err
	}
	if _, err := io.WriteString(w.lim, ddl); err != nil {
		return err
	}
	if !meta.HasData {
		return nil
	}
	var lenBuf [4]byte
	if data != nil {
		buf := make([]byte, chunkSize)
		for {
			n, rerr := data.Read(buf)
			if n > 0 {
				encbin.BigEndian.PutUint32(lenBuf[:], uint32(n))
				if _, err := w.lim.Write(lenBuf[:]); err != nil {
					return err
				}
				if _, err := w.lim.Write(buf[:n]); err != nil {
					return err
				}
			}
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				return rerr
			}
		}
	}
	// End-of-table data sentinel (0-length block).
	encbin.BigEndian.PutUint32(lenBuf[:], 0)
	_, err := w.lim.Write(lenBuf[:])
	return err
}

func (w *dumpWriter) Flush() error {
	return w.bw.Flush()
}

type dumpReader struct {
	br        *bufio.Reader
	maxTables int
	hdr       dumpHeader
	hdrRead   bool
	idx       int
}

func newDumpReader(r io.Reader, maxTables int) *dumpReader {
	if maxTables <= 0 {
		maxTables = defaultMaxDumpTables
	}
	return &dumpReader{br: bufio.NewReaderSize(r, 64<<10), maxTables: maxTables}
}

func readBoundedLine(br *bufio.Reader, maxLen int) ([]byte, error) {
	var line []byte
	for {
		frag, isPrefix, err := br.ReadLine()
		if err != nil {
			return nil, err
		}
		if len(line)+len(frag) > maxLen {
			return nil, fmt.Errorf("dump line exceeds maximum length (%d bytes)", maxLen)
		}
		line = append(line, frag...)
		if !isPrefix {
			return line, nil
		}
	}
}

func (r *dumpReader) ReadHeader() (dumpHeader, error) {
	line, err := readBoundedLine(r.br, maxDumpHeaderBytes)
	if err != nil {
		return dumpHeader{}, fmt.Errorf("read dump header: %w", err)
	}
	var hdr dumpHeader
	if err := json.Unmarshal(line, &hdr); err != nil {
		return dumpHeader{}, fmt.Errorf("parse dump header: %w", err)
	}
	if hdr.Format != dumpFormatMagic {
		return dumpHeader{}, fmt.Errorf("unsupported dump format %q", hdr.Format)
	}
	if hdr.Version != dumpFormatVersion {
		return dumpHeader{}, fmt.Errorf("unsupported dump format version %d (expected %d)", hdr.Version, dumpFormatVersion)
	}
	if len(hdr.Databases) == 0 {
		return dumpHeader{}, errors.New("dump header lists no databases")
	}
	if len(hdr.Tables) > r.maxTables {
		return dumpHeader{}, fmt.Errorf("dump header lists %d tables (limit %d)", len(hdr.Tables), r.maxTables)
	}
	dbSet := make(map[string]bool, len(hdr.Databases))
	for _, db := range hdr.Databases {
		if err := validateIdent(db); err != nil {
			return dumpHeader{}, fmt.Errorf("dump header database %q: %w", db, err)
		}
		if isForbiddenDatabase(db) {
			return dumpHeader{}, fmt.Errorf("dump header contains forbidden database %q", db)
		}
		if dbSet[db] {
			return dumpHeader{}, fmt.Errorf("dump header lists database %q twice", db)
		}
		dbSet[db] = true
	}
	for _, tbl := range hdr.Tables {
		if !dbSet[tbl.Database] {
			return dumpHeader{}, fmt.Errorf("dump header table %s.%s references unlisted database", tbl.Database, tbl.Name)
		}
		if err := validateIdent(tbl.Name); err != nil {
			return dumpHeader{}, fmt.Errorf("dump header table %s.%s: %w", tbl.Database, tbl.Name, err)
		}
		if !isAllowedTableEngine(tbl.Engine) {
			return dumpHeader{}, fmt.Errorf("dump header table %s.%s has unsupported engine %q", tbl.Database, tbl.Name, tbl.Engine)
		}
	}
	r.hdr = hdr
	r.hdrRead = true
	return hdr, nil
}

func (r *dumpReader) NextTable() (dumpTableMeta, string, *blockStreamReader, error) {
	if !r.hdrRead {
		return dumpTableMeta{}, "", nil, errors.New("dump header has not been read")
	}
	if r.idx >= len(r.hdr.Tables) {
		return dumpTableMeta{}, "", nil, io.EOF
	}
	expected := r.hdr.Tables[r.idx]
	r.idx++

	line, err := readBoundedLine(r.br, maxDumpHeaderBytes)
	if err != nil {
		return dumpTableMeta{}, "", nil, fmt.Errorf("read table record for %s.%s: %w", expected.Database, expected.Name, err)
	}
	var rec dumpTableRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return dumpTableMeta{}, "", nil, fmt.Errorf("parse table record for %s.%s: %w", expected.Database, expected.Name, err)
	}
	meta := dumpTableMeta{Database: rec.Database, Name: rec.Name, Engine: rec.Engine, HasData: rec.HasData}
	if meta != expected {
		return dumpTableMeta{}, "", nil, fmt.Errorf("table record %+v does not match header entry %+v", meta, expected)
	}
	if rec.DDLLen <= 0 || rec.DDLLen > maxDumpDDLBytes {
		return dumpTableMeta{}, "", nil, fmt.Errorf("invalid DDL length %d for %s.%s", rec.DDLLen, rec.Database, rec.Name)
	}
	ddlBuf := make([]byte, rec.DDLLen)
	if _, err := io.ReadFull(r.br, ddlBuf); err != nil {
		return dumpTableMeta{}, "", nil, fmt.Errorf("read DDL for %s.%s: %w", rec.Database, rec.Name, err)
	}
	if !meta.HasData {
		return meta, string(ddlBuf), nil, nil
	}
	return meta, string(ddlBuf), &blockStreamReader{r: r.br}, nil
}

func (r *dumpReader) VerifyEOF() error {
	if !r.hdrRead || r.idx != len(r.hdr.Tables) {
		return fmt.Errorf("incomplete dump: read %d of %d tables", r.idx, len(r.hdr.Tables))
	}
	if _, err := r.br.ReadByte(); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing data after dump")
		}
		return err
	}
	return nil
}

// blockStreamReader streams length-prefixed Native blocks from the dump until
// the 0-length sentinel. A premature EOF is reported as io.ErrUnexpectedEOF.
type blockStreamReader struct {
	r    io.Reader
	rem  uint32
	done bool
	err  error
}

// HasData peeks the first block header and reports whether the table has any
// payload bytes (false if the sentinel 0 is immediately encountered).
func (b *blockStreamReader) HasData() (bool, error) {
	if b.done {
		return false, nil
	}
	if b.err != nil {
		return false, b.err
	}
	if b.rem > 0 {
		return true, nil
	}
	if err := b.nextBlock(); err != nil {
		return false, err
	}
	return !b.done, nil
}

func (b *blockStreamReader) nextBlock() error {
	var lenBuf [4]byte
	if _, err := io.ReadFull(b.r, lenBuf[:]); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		b.err = fmt.Errorf("truncated dump block header: %w", err)
		return b.err
	}
	n := encbin.BigEndian.Uint32(lenBuf[:])
	if n == 0 {
		b.done = true
		return nil
	}
	if n > maxDumpChunkBytes {
		b.err = fmt.Errorf("dump block size %d exceeds limit %d", n, maxDumpChunkBytes)
		return b.err
	}
	b.rem = n
	return nil
}

func (b *blockStreamReader) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if b.done {
		return 0, io.EOF
	}
	if b.rem == 0 {
		if err := b.nextBlock(); err != nil {
			return 0, err
		}
		if b.done {
			return 0, io.EOF
		}
	}
	if uint32(len(p)) > b.rem {
		p = p[:b.rem]
	}
	n, err := b.r.Read(p)
	b.rem -= uint32(n)
	if err != nil {
		if errors.Is(err, io.EOF) && b.rem > 0 {
			err = io.ErrUnexpectedEOF
		}
		if !errors.Is(err, io.EOF) {
			b.err = err
			return n, err
		}
	}
	return n, nil
}

func (b *blockStreamReader) Done() bool { return b.done }
func (b *blockStreamReader) Err() error { return b.err }

// shortClickHouseError returns the concise first line (Code + exception
// summary) of a ClickHouse HTTP error body, capped at 300 bytes.
func shortClickHouseError(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "empty error response"
	}
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// scrubSecret replaces any occurrence of secret in s with "***".
func scrubSecret(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}
