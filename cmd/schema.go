package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/soltiHQ/control-plane/internal/storage/sqlstore"
)

// schema runs "podium schema version" and "podium schema export --dir DIR",
// the commands of a deployment whose migrations a tool of its own applies, as
// Bytebase, and returns the exit code. They need no configuration and reach no
// database.
func schema(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageError(stderr, errors.New("schema: version or export"))
	}
	switch args[0] {
	case "version":
		if len(args) > 1 {
			return usageError(stderr, errors.New("schema version takes no arguments"))
		}
		return schemaVersion(stdout, stderr)
	case "export":
		dir, ok := schemaExportArgs(args[1:], stderr)
		if !ok {
			return 2
		}
		return schemaExport(dir, stdout, stderr)
	}
	return usageError(stderr, fmt.Errorf("schema: unknown subcommand %q", args[0]))
}

// schemaVersion prints the version of podium's newest migration
// (sqlstore.LatestVersion) and a newline: the schema version this podium runs
// on, which a database migrated by another tool reaches before this podium
// starts on it with cluster.database.migrate off.
func schemaVersion(stdout, stderr io.Writer) int {
	latest, err := sqlstore.LatestVersion(sqlstore.Postgres{})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "podium: schema version: %v\n", err)
		return 1
	}
	if _, err := fmt.Fprintln(stdout, latest); err != nil {
		_, _ = fmt.Fprintf(stderr, "podium: schema version: write the version: %v\n", err)
		return 1
	}
	return 0
}

// schemaExportArgs reads the command line of "podium schema export": --dir DIR,
// which it requires, in any spelling of the flag. Anything else prints the
// error and the usage to stderr and reports false.
func schemaExportArgs(args []string, stderr io.Writer) (string, bool) {
	var (
		dir   string
		flags = flag.NewFlagSet("podium schema export", flag.ContinueOnError)
	)
	flags.SetOutput(io.Discard)
	flags.StringVar(&dir, "dir", "", "")
	if err := flags.Parse(args); err != nil {
		usageError(stderr, fmt.Errorf("schema export: %w", err))
		return "", false
	}
	switch {
	case flags.NArg() > 0:
		usageError(stderr, fmt.Errorf("schema export: unexpected argument %q", flags.Arg(0)))
		return "", false
	case dir == "":
		usageError(stderr, errors.New("schema export: --dir is required"))
		return "", false
	}
	return dir, true
}

// schemaExport writes podium's migrations into dir, which it makes when it is
// missing, each file as podium applies it, and returns the exit code. It never
// overwrites a file: one of a migration's name with the same content stays, and
// so does a file of no migration's name; one of a migration's name with other
// content stops the export before it writes any file, exit 1, as a migration
// never changes once released, and a copy that differs is for a person to look
// at. It names each file it wrote or kept on stdout, and an error on stderr.
func schemaExport(dir string, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "podium: schema export: %v\n", err)
		return 1
	}
	d := sqlstore.Postgres{}
	// The files, checked as the migrator checks them.
	if _, err := sqlstore.LatestVersion(d); err != nil {
		return fail(err)
	}
	entries, err := fs.ReadDir(d.Migrations(), ".")
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(err)
	}
	type file struct {
		path  string
		body  []byte
		there bool // the same file is in dir
	}
	var (
		files  []file
		differ []string
	)
	for _, e := range entries {
		body, err := fs.ReadFile(d.Migrations(), e.Name())
		if err != nil {
			return fail(err)
		}
		path := filepath.Join(dir, e.Name())
		switch existing, err := os.ReadFile(path); {
		case errors.Is(err, fs.ErrNotExist):
			files = append(files, file{path: path, body: body})
		case err != nil:
			return fail(err)
		case bytes.Equal(existing, body):
			files = append(files, file{path: path, body: body, there: true})
		default:
			differ = append(differ, path)
		}
	}
	if len(differ) > 0 {
		return fail(fmt.Errorf("%s: other content than podium's migration of that name; nothing was written", strings.Join(differ, ", ")))
	}
	for _, f := range files {
		if f.there {
			_, _ = fmt.Fprintf(stdout, "kept %s\n", f.path)
			continue
		}
		if err := writeNew(f.path, f.body); err != nil {
			return fail(err)
		}
		_, _ = fmt.Fprintf(stdout, "wrote %s\n", f.path)
	}
	return 0
}

// writeNew writes body to a new file at path, mode 0644, and fails when a file
// is there already; a write that fails removes the file it began.
func writeNew(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(body)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}
