package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"sort"
	"strconv"
	"strings"

	pb "github.com/db-branch/db-branch/pkg/ext/proto/runtime/v1"
)

// esc makes s safe as XML text.
func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// serverConfig is config.xml of an instance. Only the HTTP interface is
// opened (the one port db-branch allocates); the native, interserver, MySQL and
// PostgreSQL interfaces, clustering and crash-report uploads stay off. In
// maintenance the server listens on the loopback address only: ClickHouse has
// no way to run without a network.
func serverConfig(i *pb.InstanceSpec, maintenance bool) string {
	listen := i.Bind
	if maintenance || listen == "" {
		listen = "127.0.0.1"
	}
	data := strings.TrimSuffix(i.DataDir, "/") + "/"
	var b strings.Builder
	b.WriteString("<clickhouse>\n")
	fmt.Fprintf(&b, "  <logger><level>information</level><log>%s/clickhouse-server.log</log><errorlog>%s/clickhouse-server.err.log</errorlog><size>100M</size><count>3</count></logger>\n", esc(i.StateDir), esc(i.StateDir))
	fmt.Fprintf(&b, "  <listen_host>%s</listen_host>\n", esc(listen))
	fmt.Fprintf(&b, "  <http_port>%d</http_port>\n", i.Ports["http"])
	fmt.Fprintf(&b, "  <path>%s</path>\n", esc(data))
	fmt.Fprintf(&b, "  <tmp_path>%stmp/</tmp_path>\n", esc(data))
	fmt.Fprintf(&b, "  <user_files_path>%suser_files/</user_files_path>\n", esc(data))
	fmt.Fprintf(&b, "  <format_schema_path>%sformat_schemas/</format_schema_path>\n", esc(data))
	fmt.Fprintf(&b, "  <access_control_path>%saccess/</access_control_path>\n", esc(data))
	fmt.Fprintf(&b, "  <users_config>%s/users.xml</users_config>\n", esc(i.StateDir))
	b.WriteString("  <default_profile>default</default_profile>\n  <default_database>default</default_database>\n")
	b.WriteString("  <timezone>UTC</timezone>\n  <mlock_executable>false</mlock_executable>\n")
	b.WriteString("  <send_crash_reports><enabled>false</enabled></send_crash_reports>\n")
	if mb := i.GetLimits().GetMemoryMb(); mb > 0 {
		// The branch's memory setting is a cache budget; the server needs room of its own
		// (about 140 MiB idle). The unit allows MemoryMB*2+512 MiB.
		fmt.Fprintf(&b, "  <max_server_memory_usage>%d</max_server_memory_usage>\n", (int64(mb)*2+384)<<20)
	}
	b.WriteString("</clickhouse>\n")
	return b.String()
}

// loginGrants is what a branch's login may do: read and change the data and
// the objects in it. No access management, no SYSTEM, and none of the grants
// that reach outside the server (FILE, URL, REMOTE, S3, ...).
const loginGrants = "GRANT SELECT, INSERT, ALTER, CREATE, DROP, TRUNCATE, OPTIMIZE, SHOW ON *.*"

// usersXML is users.xml: an admin only the node itself can reach, and the
// logins by the SHA-256 of their passwords, in name order.
func usersXML(logins map[string]string) string {
	names := make([]string, 0, len(logins))
	for n := range logins {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("<clickhouse>\n  <profiles><default/></profiles>\n  <quotas><default/></quotas>\n  <users>\n")
	b.WriteString("    <admin><password></password><networks><ip>127.0.0.1</ip><ip>::1</ip></networks><profile>default</profile><quota>default</quota><access_management>1</access_management></admin>\n")
	for _, n := range names {
		name := esc(n)
		fmt.Fprintf(&b, "    <%s><password_sha256_hex>%s</password_sha256_hex><networks><ip>::/0</ip></networks><profile>default</profile><quota>default</quota><grants><query>%s</query></grants></%s>\n",
			name, esc(logins[n]), loginGrants, name)
	}
	b.WriteString("  </users>\n</clickhouse>\n")
	return b.String()
}

func hashPassword(p string) string {
	sum := sha256.Sum256([]byte(p))
	return hex.EncodeToString(sum[:])
}

// parseResult reads ClickHouse's JSONCompact output. maxRows 0 is no limit.
func parseResult(body []byte, maxRows int) (*pb.QueryResponse, error) {
	var doc struct {
		Meta []struct {
			Name string `json:"name"`
		} `json:"meta"`
		Data [][]any `json:"data"`
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if err := d.Decode(&doc); err != nil {
		return nil, fmt.Errorf("unexpected answer from ClickHouse: %.200s", strings.TrimSpace(string(body)))
	}
	r := &pb.QueryResponse{}
	for _, m := range doc.Meta {
		r.Columns = append(r.Columns, m.Name)
	}
	for n, row := range doc.Data {
		if maxRows > 0 && n >= maxRows {
			r.Truncated = true
			break
		}
		out := &pb.Row{}
		for _, v := range row {
			out.Cells = append(out.Cells, cell(v))
		}
		r.Rows = append(r.Rows, out)
	}
	return r, nil
}

func cell(v any) *pb.Cell {
	switch x := v.(type) {
	case nil:
		return &pb.Cell{IsNull: true}
	case string:
		return &pb.Cell{Value: x}
	case json.Number:
		return &pb.Cell{Value: x.String()}
	case bool:
		return &pb.Cell{Value: strconv.FormatBool(x)}
	}
	b, _ := json.Marshal(v)
	return &pb.Cell{Value: string(b)}
}

// splitStatements splits a script at semicolons outside quotes, dropping
// comments and blank statements (the HTTP interface runs one statement at a time).
func splitStatements(sql string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			j := i + 1
			for j < len(sql) && sql[j] != c {
				if sql[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(sql) {
				j = len(sql) - 1
			}
			cur.WriteString(sql[i : j+1])
			i = j
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				i = len(sql)
			} else {
				i += end + 3
			}
		case c == ';':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// freezeState is carried in FreezeToken.Data so PostFreeze knows which instance
// was frozen and what PreFreeze stopped.
type freezeState struct {
	Host          string `json:"host,omitempty"`
	Port          int32  `json:"port,omitempty"`
	StoppedMerges bool   `json:"stopped_merges"`
	StoppedMoves  bool   `json:"stopped_moves"`
}

func encodeFreezeToken(st freezeState) string {
	b, _ := json.Marshal(st)
	return string(b)
}

func decodeFreezeToken(data string) (freezeState, bool) {
	if strings.TrimSpace(data) == "" {
		return freezeState{}, false
	}
	var st freezeState
	if err := json.Unmarshal([]byte(data), &st); err != nil {
		return freezeState{}, false
	}
	return st, true
}

// parseCounts reads want non-negative integer counts from the first row of a
// ClickHouse JSONCompact response (UInt64 values arrive as JSON strings).
func parseCounts(body []byte, want int) ([]int64, error) {
	res, err := parseResult(body, 0)
	if err != nil {
		return nil, err
	}
	if len(res.Rows) == 0 || len(res.Rows[0].Cells) < want {
		return nil, fmt.Errorf("unexpected row shape from ClickHouse: got %d row(s)", len(res.Rows))
	}
	out := make([]int64, want)
	for i := 0; i < want; i++ {
		c := res.Rows[0].Cells[i]
		if c.IsNull {
			return nil, fmt.Errorf("unexpected NULL at column %d", i)
		}
		n, err := strconv.ParseInt(strings.TrimSpace(c.Value), 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid count %q at column %d", c.Value, i)
		}
		out[i] = n
	}
	return out, nil
}

// parseCHVersion parses a ClickHouse numeric version string (e.g. "26.3.19.3"
// or "1.0", matching Describe.VersionPattern).
func parseCHVersion(s string) ([]int, error) {
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid ClickHouse version %q", s)
	}
	nums := make([]int, len(parts))
	for i, p := range parts {
		if p == "" {
			return nil, fmt.Errorf("invalid ClickHouse version %q", s)
		}
		for j := 0; j < len(p); j++ {
			if p[j] < '0' || p[j] > '9' {
				return nil, fmt.Errorf("invalid ClickHouse version %q", s)
			}
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid ClickHouse version %q: %w", s, err)
		}
		nums[i] = n
	}
	return nums, nil
}

func compareCHVersions(a, b []int) int {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		var va, vb int
		if i < len(a) {
			va = a[i]
		}
		if i < len(b) {
			vb = b[i]
		}
		if va < vb {
			return -1
		}
		if va > vb {
			return 1
		}
	}
	return 0
}

// checkUpgradePath allows moving to a strictly newer ClickHouse version and
// refuses the same version or any downgrade.
func checkUpgradePath(from, to string) error {
	a, err := parseCHVersion(from)
	if err != nil {
		return fmt.Errorf("from version: %w", err)
	}
	b, err := parseCHVersion(to)
	if err != nil {
		return fmt.Errorf("to version: %w", err)
	}
	switch compareCHVersions(a, b) {
	case 0:
		return fmt.Errorf("already running ClickHouse %s", from)
	case 1:
		return fmt.Errorf("downgrade from ClickHouse %s to %s is not supported", from, to)
	}
	return nil
}

func isClickHouseLTS(month int) bool { return month == 3 || month == 8 }

// ltsBefore returns the count of ClickHouse LTS releases (.3, .8) strictly before (y, m).
func ltsBefore(y, m int) int {
	switch {
	case m <= 3:
		return 2 * y
	case m <= 8:
		return 2*y + 1
	default:
		return 2*y + 2
	}
}

// ltsAtMost returns the count of ClickHouse LTS releases (.3, .8) at or before (y, m).
func ltsAtMost(y, m int) int {
	switch {
	case m < 3:
		return 2 * y
	case m < 8:
		return 2*y + 1
	default:
		return 2*y + 2
	}
}

// upgradePathWarnings returns non-blocking warnings when an upgrade skips
// ClickHouse LTS releases, targets a non-LTS release, or skips intermediate
// minor releases from a non-LTS release.
func upgradePathWarnings(from, to string) []*pb.Finding {
	a, err1 := parseCHVersion(from)
	b, err2 := parseCHVersion(to)
	if err1 != nil || err2 != nil || compareCHVersions(a, b) >= 0 {
		return nil
	}
	fy, fm := a[0], a[1]
	ty, tm := b[0], b[1]
	if fy == ty && fm == tm {
		return nil
	}
	if fm < 1 || fm > 12 || tm < 1 || tm > 12 {
		return nil
	}
	var out []*pb.Finding
	if skipped := ltsBefore(ty, tm) - ltsAtMost(fy, fm); skipped > 0 {
		start := ltsAtMost(fy, fm)
		const maxListed = 4
		show := skipped
		if show > maxListed {
			show = maxListed
		}
		names := make([]string, 0, show)
		for k := 0; k < show; k++ {
			idx := start + k
			month := 3
			if idx%2 == 1 {
				month = 8
			}
			names = append(names, fmt.Sprintf("%d.%d", idx/2, month))
		}
		summary := strings.Join(names, ", ")
		if skipped > maxListed {
			summary += fmt.Sprintf(" and %d more", skipped-maxListed)
		}
		out = append(out, &pb.Finding{
			Level:   "warning",
			Code:    "upgrade_path",
			Message: fmt.Sprintf("upgrading ClickHouse from %s to %s skips LTS release(s) (%s); review release notes for breaking changes", from, to, summary),
		})
	}
	if !isClickHouseLTS(tm) {
		out = append(out, &pb.Finding{
			Level:   "warning",
			Code:    "upgrade_path",
			Message: fmt.Sprintf("ClickHouse %s is not an LTS release (LTS releases are .3 and .8); review release notes for minor version changes", to),
		})
	} else if len(out) == 0 && !isClickHouseLTS(fm) && (ty*12+tm)-(fy*12+fm) > 1 {
		out = append(out, &pb.Finding{
			Level:   "warning",
			Code:    "upgrade_path",
			Message: fmt.Sprintf("upgrading ClickHouse from %s to %s skips intermediate minor releases; review release notes for breaking changes", from, to),
		})
	}
	return out
}
