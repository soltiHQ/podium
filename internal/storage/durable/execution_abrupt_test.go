package durable

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDurableExecutionArchiveSurvivesAbruptExit(t *testing.T) {
	const childEnv = "SOLTI_DURABLE_ARCHIVE_CRASH_CHILD"
	if dir := os.Getenv(childEnv); dir != "" {
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.UpsertExecution(context.Background(), durableArchiveRow(t)); err != nil {
			t.Fatal(err)
		}
		os.Exit(0) // Intentionally skip Close and all deferred shutdown work.
	}
	dir := filepath.Join(t.TempDir(), "crash-state")
	command := exec.Command(os.Args[0], "-test.run=^TestDurableExecutionArchiveSurvivesAbruptExit$")
	command.Env = append(os.Environ(), childEnv+"="+dir)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("abrupt child: %v\n%s", err, out)
	}
	restored, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	want := durableArchiveRow(t)
	got, err := restored.GetExecution(context.Background(), want.ID())
	if err != nil || !reflect.DeepEqual(got.Params(), want.Params()) {
		t.Fatal("acknowledged terminal archive lost after abrupt exit", err)
	}
}
