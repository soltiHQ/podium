// Package migrations holds podium's SQL migrations: the schema of the database
// mode, as files of their own, which podium applies itself (podium migrate, or
// each replica at its start) and which a tool of the deployment's own, as
// Bytebase, can apply as well (podium schema export writes them out). The
// contract of the files is in README.md.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed postgres/*.sql
var files embed.FS

// postgres is the postgres directory of files, at the root.
var postgres = func() fs.FS {
	sub, err := fs.Sub(files, "postgres")
	if err != nil {
		// fs.Sub fails for an invalid path alone, and this one is a constant.
		panic(err)
	}
	return sub
}()

// Postgres returns the migrations of the Postgres schema: NNNN_name.sql at its
// root, versions 1, 2, 3… without a gap.
func Postgres() fs.FS { return postgres }
