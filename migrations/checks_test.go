package migrations

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The checks of the migration files are textual: they read the statements of a
// file as words, and refuse the ones a migration must not run. They know the
// SQL podium's files use: no dollar quoting, which they refuse to read, and no
// escape string (E'…'), which they misread.

// statements returns the statements of a migration file: without its -- and
// /* */ comments and the content of its string literals, spacing collapsed,
// upper-cased, a quoted identifier as it is. It refuses dollar quoting and a
// literal, identifier or comment left open.
func statements(sql string) ([]string, error) {
	var (
		b   strings.Builder
		all []string
	)
	flush := func() {
		if s := strings.Join(strings.Fields(b.String()), " "); s != "" {
			all = append(all, strings.ToUpper(s))
		}
		b.Reset()
	}
	for i := 0; i < len(sql); i++ {
		switch c := sql[i]; {
		case strings.HasPrefix(sql[i:], "--"):
			end := strings.IndexByte(sql[i:], '\n')
			if end < 0 {
				i = len(sql)
				continue
			}
			i += end
			b.WriteByte(' ')
		case strings.HasPrefix(sql[i:], "/*"):
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return nil, errors.New("a /* comment left open")
			}
			i += 2 + end + 1
			b.WriteByte(' ')
		case c == '\'' || c == '"':
			// A doubled quote within it is a quote, and the run goes on.
			end := i + 1
			for ; end < len(sql); end++ {
				if sql[end] == c {
					if end+1 < len(sql) && sql[end+1] == c {
						end++
						continue
					}
					break
				}
			}
			if end >= len(sql) {
				return nil, errors.New("a literal or a quoted identifier left open")
			}
			if c == '"' {
				b.WriteString(sql[i : end+1])
			} else {
				b.WriteString("''")
			}
			i = end
		case c == '$':
			return nil, errors.New("dollar quoting, which the checks cannot read")
		case c == ';':
			flush()
		default:
			b.WriteByte(c)
		}
	}
	flush()
	return all, nil
}

var (
	// transactionControl are the first words of the statements that begin,
	// end or split a transaction.
	transactionControl = []string{"BEGIN", "START", "COMMIT", "END", "ROLLBACK", "ABORT", "SAVEPOINT", "RELEASE", "PREPARE"}

	// contracts finds, in an ALTER statement, what removes or changes what
	// exists: a drop, a rename, a new type, a column made NOT NULL.
	contracts = regexp.MustCompile(`\b(DROP|RENAME)\b|\bSET NOT NULL\b|\bALTER (COLUMN )?\S+ (SET DATA )?TYPE\b`)

	// addsNotNull finds, in an ALTER statement, a column added NOT NULL.
	addsNotNull = regexp.MustCompile(`\bADD (COLUMN )?.*\bNOT NULL\b`)
)

// breaks returns the statements, as statements returns them, that break the
// contract of a migration file, each with what it breaks: a file runs in one
// transaction, whoever applies it, so it holds no transaction control and
// nothing that runs outside a transaction; and the podium before it still runs
// on the schema it leaves, during a rolling update, so it only adds: no drop,
// truncation, deletion, rename or new type, and no column made NOT NULL, or
// added NOT NULL without a default.
func breaks(statements []string) []string {
	var found []string
	for _, s := range statements {
		first, _, _ := strings.Cut(s, " ")
		alter := first == "ALTER"
		switch {
		case slices.Contains(transactionControl, first):
			found = append(found, "transaction control: "+s)
		case first == "VACUUM" || slices.Contains(strings.Fields(s), "CONCURRENTLY"):
			found = append(found, "runs outside a transaction: "+s)
		case slices.Contains([]string{"DROP", "TRUNCATE", "DELETE"}, first), alter && contracts.MatchString(s):
			found = append(found, "removes or changes what the podium before uses: "+s)
		case alter && addsNotNull.MatchString(s) && !strings.Contains(s, " DEFAULT "):
			found = append(found, "adds a NOT NULL column without a default, which the writes of the podium before leave out: "+s)
		}
	}
	return found
}

// TestStatements checks how the checks read a file: statement by statement,
// without comments or the content of string literals, spacing collapsed and
// upper-cased; a file they cannot read is an error.
func TestStatements(t *testing.T) {
	got, err := statements("-- BEGIN; in a comment\nCREATE TABLE a (\n  id   int -- it's the key; really\n);\n/* DROP TABLE a; */ COMMENT ON TABLE a IS 'drop; it''s; begin';\nCREATE TABLE \"b;c\" (id int);\n")
	want := []string{"CREATE TABLE A ( ID INT )", "COMMENT ON TABLE A IS ''", `CREATE TABLE "B;C" (ID INT)`}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("statements: %q, %v; want %q", got, err, want)
	}
	for _, sql := range []string{
		"DO $$ BEGIN END $$;",
		"COMMENT ON TABLE a IS 'open;",
		"CREATE TABLE a (id int); /* open",
		`CREATE TABLE "a (id int);`,
	} {
		if got, err := statements(sql); err == nil {
			t.Errorf("statements(%q) = %q, want an error", sql, got)
		}
	}
}

// TestBreaks checks what the checks refuse in a migration file: transaction
// control, statements that run outside a transaction, and statements that
// remove or change what the podium before still uses.
func TestBreaks(t *testing.T) {
	for _, sql := range []string{
		"BEGIN", "START TRANSACTION", "COMMIT", "END", "ROLLBACK", "ABORT",
		"SAVEPOINT s", "RELEASE SAVEPOINT s", "ROLLBACK TO SAVEPOINT s", "PREPARE TRANSACTION 'x'",
		"CREATE INDEX CONCURRENTLY t_c ON t (c)", "REINDEX INDEX CONCURRENTLY t_c", "VACUUM t",
		"DROP TABLE t", "DROP INDEX t_c", "TRUNCATE t", "DELETE FROM t",
		"ALTER TABLE t DROP COLUMN c", "ALTER TABLE t DROP CONSTRAINT t_c",
		"ALTER TABLE t RENAME COLUMN a TO b", "ALTER TABLE t RENAME TO u",
		"ALTER TABLE t ALTER COLUMN c TYPE bigint", "ALTER TABLE t ALTER c SET DATA TYPE bigint",
		"ALTER TABLE t ALTER COLUMN c SET NOT NULL", "ALTER TABLE t ADD COLUMN c int NOT NULL",
	} {
		stmts, err := statements(sql + ";")
		if err != nil {
			t.Fatalf("statements(%q): %v", sql, err)
		}
		if got := breaks(stmts); len(got) != 1 {
			t.Errorf("breaks(%q) = %q, want one", sql, got)
		}
	}
	stmts, err := statements(`CREATE TABLE t (id int NOT NULL, ref bytea REFERENCES u (id) ON DELETE CASCADE);
CREATE INDEX t_ref ON t (ref);
ALTER TABLE t ADD COLUMN c int NOT NULL DEFAULT 0;
ALTER TABLE t ADD COLUMN d text;
ALTER TABLE t ALTER COLUMN d DROP NOT NULL;
UPDATE t SET d = 'x' WHERE d IS NULL;
INSERT INTO podium_schema_migrations (version) VALUES (2) ON CONFLICT (version) DO NOTHING;`)
	if err != nil {
		t.Fatal(err)
	}
	// Dropping NOT NULL relaxes a column, which the podium before writes all
	// the same, and yet it is refused: the checks are textual, and refuse
	// every DROP of an ALTER.
	want := []string{"removes or changes what the podium before uses: ALTER TABLE T ALTER COLUMN D DROP NOT NULL"}
	if got := breaks(stmts); !slices.Equal(got, want) {
		t.Errorf("breaks of additive statements: %q, want %q", got, want)
	}
	if got := strings.Join(breaks(nil), ""); got != "" {
		t.Errorf("breaks(nil) = %q", got)
	}
}
