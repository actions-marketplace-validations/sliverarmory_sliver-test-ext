package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bishopfox/sliver/protobuf/clientpb"
	"github.com/bishopfox/sliver/protobuf/commonpb"
	"github.com/bishopfox/sliver/protobuf/sliverpb"
	"google.golang.org/protobuf/proto"
)

func TestReadLibraryChecksBytesAndSHA256(t *testing.T) {
	contents := []byte("native library bytes")
	path := filepath.Join(t.TempDir(), "extension.so")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	want := hex.EncodeToString(digest[:])

	got, err := readLibrary(Case{LibraryPath: path, LibrarySHA256: want})
	if err != nil || string(got) != string(contents) {
		t.Fatalf("readLibrary() = %q, %v; want %q", got, err, contents)
	}
	if _, err := readLibrary(Case{LibraryData: contents, LibrarySHA256: strings.Repeat("0", 64)}); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("readLibrary() digest mismatch = %v", err)
	}
	if _, err := readLibrary(Case{LibraryData: []byte{}}); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("readLibrary() empty data = %v", err)
	}
}

func TestValidateRunOptionsRejectsWrongCaseTarget(t *testing.T) {
	server := filepath.Join(t.TempDir(), "sliver-server")
	if err := os.WriteFile(server, []byte("server"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := validateRunOptions(Options{
		Workspace: t.TempDir(), ServerPath: server, ResultsDir: t.TempDir(),
		Target: Target{OS: "linux", Arch: "amd64"},
		Cases:  []Case{{Name: "echo", Target: Target{OS: "windows", Arch: "amd64"}, Export: "Echo", Repeat: 1}},
	})
	if err == nil || !strings.Contains(err.Error(), "targets windows/amd64") {
		t.Fatalf("validateRunOptions() = %v; want target mismatch", err)
	}
}

func TestDecodeBeaconTaskRequiresExactCompletedTask(t *testing.T) {
	responseData, err := proto.Marshal(&sliverpb.CallExtension{
		Output:   []byte("ok"),
		Response: &commonpb.Response{},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := &clientpb.BeaconTask{
		ID: "task-1", BeaconID: "beacon-1", State: "completed", CompletedAt: 1, Response: responseData,
	}
	destination := &sliverpb.CallExtension{}
	if err := decodeBeaconTask(task, "task-1", "beacon-1", destination); err != nil {
		t.Fatal(err)
	}
	if string(destination.Output) != "ok" || destination.Response == nil {
		t.Fatalf("decoded response = %#v", destination)
	}
	if !beaconTaskCompleted(task, "task-1", "beacon-1") {
		t.Fatal("completed task was not recognized")
	}

	task.BeaconID = "other"
	if err := decodeBeaconTask(task, "task-1", "beacon-1", destination); err == nil || !strings.Contains(err.Error(), "correlation mismatch") {
		t.Fatalf("correlation error = %v", err)
	}
	if beaconTaskCompleted(task, "task-1", "beacon-1") {
		t.Fatal("mismatched task was recognized as completed")
	}
}
