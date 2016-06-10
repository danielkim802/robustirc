package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/robustirc/robustirc/config"
	"github.com/robustirc/robustirc/ircserver"
	"github.com/robustirc/robustirc/raft_store"
	"github.com/robustirc/robustirc/types"
	"github.com/stapelberg/glog"

	"github.com/hashicorp/raft"
)

func appendLog(logs []*raft.Log, msg string) []*raft.Log {
	return append(logs, &raft.Log{
		Type:  raft.LogCommand,
		Index: uint64(len(logs) + 1),
		Data:  []byte(msg),
	})
}

func verifyEndState(t *testing.T) {
	s, err := ircServer.GetSession(types.RobustId{Id: 1})
	if err != nil {
		t.Fatalf("No session found after applying log messages")
	}
	if s.Nick != "secure_" {
		t.Fatalf("session.Nick: got %q, want %q", s.Nick, "secure_")
	}

	// s.Channels is a map[lcChan]bool, so we copy it over.
	got := make(map[string]bool)
	for key, value := range s.Channels {
		got[string(key)] = value
	}

	want := make(map[string]bool)
	want["#chaos-hd"] = true

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("session.Channels: got %v, want %v", got, want)
	}
}

// TestCompaction does a full snapshot, persists it to disk, restores it and
// makes sure the state matches expectations. The other test functions directly
// test what should be compacted.
func TestCompaction(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	tempdir, err := ioutil.TempDir("", "robust-test-")
	if err != nil {
		t.Fatalf("ioutil.TempDir: %v", err)
	}
	defer os.RemoveAll(tempdir)

	flag.Set("raftdir", tempdir)

	logstore, err := raft_store.NewLevelDBStore(filepath.Join(tempdir, "raftlog"), false)
	if err != nil {
		t.Fatalf("Unexpected error in NewLevelDBStore: %v", err)
	}
	ircstore, err := raft_store.NewLevelDBStore(filepath.Join(tempdir, "irclog"), false)
	if err != nil {
		t.Fatalf("Unexpected error in NewLevelDBStore: %v", err)
	}
	fsm := FSM{logstore, ircstore}

	var logs []*raft.Log
	logs = appendLog(logs, `{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`)
	logs = appendLog(logs, `{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`)
	logs = appendLog(logs, `{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`)
	logs = appendLog(logs, `{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK secure_"}`)
	logs = appendLog(logs, `{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd"}`)
	logs = appendLog(logs, `{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #i3"}`)
	logs = appendLog(logs, `{"Id": {"Id": 7}, "Session": {"Id": 1}, "Type": 2, "Data": "PRIVMSG #chaos-hd :heya"}`)
	logs = appendLog(logs, `{"Id": {"Id": 8}, "Session": {"Id": 1}, "Type": 2, "Data": "PRIVMSG #chaos-hd :newer message"}`)
	logs = appendLog(logs, `{"Id": {"Id": 9}, "Session": {"Id": 1}, "Type": 2, "Data": "PART #i3"}`)

	// These messages are too new to be compacted.
	nowID := time.Now().UnixNano()
	logs = appendLog(logs, `{"Id": {"Id": `+strconv.FormatInt(nowID, 10)+`}, "Session": {"Id": 1}, "Type": 2, "Data": "PART #chaos-hd"}`)
	nowID++
	logs = appendLog(logs, `{"Id": {"Id": `+strconv.FormatInt(nowID, 10)+`}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd"}`)

	if err := logstore.StoreLogs(logs); err != nil {
		t.Fatalf("Unexpected error in store.StoreLogs: %v", err)
	}
	for _, log := range logs {
		fsm.Apply(log)
	}

	verifyEndState(t)

	snapshot, err := fsm.Snapshot()
	if err != nil {
		t.Fatalf("Unexpected error in fsm.Snapshot(): %v", err)
	}

	robustsnap, ok := snapshot.(*robustSnapshot)
	if !ok {
		t.Fatalf("fsm.Snapshot() return value is not a robustSnapshot")
	}
	if robustsnap.lastIndex != uint64(len(logs)) {
		t.Fatalf("snapshot does not retain the last message, got: %d, want: %d", robustsnap.lastIndex, len(logs))
	}

	fss, err := raft.NewFileSnapshotStore(tempdir, 5, nil)
	if err != nil {
		t.Fatalf("%v", err)
	}

	sink, err := fss.Create(uint64(len(logs)), 1, []byte{})
	if err != nil {
		t.Fatalf("fss.Create: %v", err)
	}

	if err := snapshot.Persist(sink); err != nil {
		t.Fatalf("Unexpected error in snapshot.Persist(): %v", err)
	}
	sink.Close()

	snapshots, err := fss.List()
	if err != nil {
		t.Fatalf("fss.List(): %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("len(snapshots): got %d, want 1", len(snapshots))
	}
	_, readcloser, err := fss.Open(snapshots[0].ID)
	if err != nil {
		t.Fatalf("fss.Open(%s): %v", snapshots[0].ID, err)
	}

	if err := fsm.Restore(readcloser); err != nil {
		t.Fatalf("fsm.Restore(): %v", err)
	}

	first, _ := fsm.ircstore.FirstIndex()
	last, _ := fsm.ircstore.LastIndex()

	if last-first >= uint64(len(logs)) {
		t.Fatalf("Compaction did not decrease log size. got: %d, want: < %d", last-first, len(logs))
	}

	verifyEndState(t)

	// Try doing repeated snapshots to catch errors in cleaning up the VIEWs.
	snapshot, err = fsm.Snapshot()
	if err != nil {
		t.Fatalf("Unexpected error in fsm.Snapshot(): %v", err)
	}

	if err := snapshot.Persist(&raft.DiscardSnapshotSink{}); err != nil {
		t.Fatalf("Unexpected error in snapshot.Persist(): %v", err)
	}
	sink.Close()

	snapshot, err = fsm.Snapshot()
	if err != nil {
		t.Fatalf("Unexpected error in fsm.Snapshot(): %v", err)
	}

	if err := snapshot.Persist(&raft.DiscardSnapshotSink{}); err != nil {
		t.Fatalf("Unexpected error in snapshot.Persist(): %v", err)
	}
	sink.Close()
}

type inMemorySink struct {
	b bytes.Buffer
}

func (s *inMemorySink) Write(p []byte) (n int, err error) {
	n, err = s.b.Write(p)
	return
}

func (s *inMemorySink) Close() error {
	return nil
}

func (s *inMemorySink) ID() string {
	return "inmemory"
}

func (s *inMemorySink) Cancel() error {
	return nil
}

func applyAndCompact(t *testing.T, input []string) []string {
	tempdir, err := ioutil.TempDir("", "robust-test-")
	if err != nil {
		t.Fatalf("ioutil.TempDir: %v", err)
	}
	defer os.RemoveAll(tempdir)

	logstore, err := raft_store.NewLevelDBStore(filepath.Join(tempdir, "raftlog"), false)
	if err != nil {
		t.Fatalf("Unexpected error in NewLevelDBStore: %v", err)
	}
	defer logstore.Close()
	ircstore, err := raft_store.NewLevelDBStore(filepath.Join(tempdir, "irclog"), false)
	if err != nil {
		t.Fatalf("Unexpected error in NewLevelDBStore: %v", err)
	}
	defer ircstore.Close()
	fsm := FSM{logstore, ircstore}

	var logs []*raft.Log
	for _, msg := range input {
		logs = append(logs, &raft.Log{
			Type:  raft.LogCommand,
			Index: uint64(len(logs) + 1),
			Data:  []byte(msg),
		})
	}

	if err := logstore.StoreLogs(logs); err != nil {
		t.Fatalf("Unexpected error in store.StoreLogs: %v", err)
	}

	for _, log := range logs {
		fsm.Apply(log)
	}

	rawsnap, err := fsm.Snapshot()
	if err != nil {
		t.Fatalf("Unexpected error in fsm.Snapshot: %v", err)
	}
	s := rawsnap.(*robustSnapshot)
	sink := inMemorySink{}
	if err := s.Persist(&sink); err != nil {
		t.Fatalf("Unexpected error in Persist: %v", err)
	}

	dec := json.NewDecoder(&sink.b)
	var output []string
	for {
		var l raft.Log
		if err := dec.Decode(&l); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("Unexpected error in json.Decode: %v", err)
		}
		output = append(output, string(l.Data))
	}

	return output
}

func mustMatchStrings(t *testing.T, input []string, got []string, want []string) {
	if (len(want) == 0 && len(got) == 0) || reflect.DeepEqual(got, want) {
		return
	}

	t.Logf("input (%d messages):\n", len(input))
	for _, msg := range input {
		t.Logf("    %s\n", msg)
	}
	t.Logf("output (%d messages):\n", len(got))
	for _, msg := range got {
		t.Logf("    %s\n", msg)
	}
	t.Logf("expected (%d messages):\n", len(want))
	for _, msg := range want {
		t.Logf("    %s\n", msg)
	}
	t.Fatalf("compacted output does not match expectation: got %v, want %v\n", got, want)
}

func TestCompactNickNone(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK secure2"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "TOPIC #chaos-hd :foo"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK secure3"}`,
	}

	output := applyAndCompact(t, input)
	// Nothing can be compacted: the first NICK is necessary so that the
	// session is loggedIn() for further messages, the second NICK is necessary
	// so that #chaos-hd’s topicNick has the correct value.
	mustMatchStrings(t, input, output, input)
}

func TestCompactNickOne(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK secure2"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "TOPIC #chaos-hd :foo"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK secure3"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 1}, "Type": 2, "Data": "TOPIC #chaos-hd :bar"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK secure3"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 1}, "Type": 2, "Data": "TOPIC #chaos-hd :bar"}`,
	}

	output := applyAndCompact(t, input)
	// Only the first and third NICK remain.
	mustMatchStrings(t, input, output, want)
}

func TestJoinPart(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "PART #chaos-hd"}`,
	}

	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
	}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactTopic(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "TOPIC #chaos-hd :foo"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 1}, "Type": 2, "Data": "TOPIC #chaos-hd :blah"}`,
	}

	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 1}, "Type": 2, "Data": "TOPIC #chaos-hd :blah"}`,
	}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestJoinTopic(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "PRIVMSG #chaos-hd :blah"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 1}, "Type": 2, "Data": "TOPIC #chaos-hd :foo"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 1}, "Type": 2, "Data": "PART #chaos-hd"}`,
	}

	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		// The JOIN must be retained, otherwise TOPIC cannot succeed.
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 1}, "Type": 2, "Data": "TOPIC #chaos-hd :foo"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 1}, "Type": 2, "Data": "PART #chaos-hd"}`,
	}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactDoubleJoin(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "PART #chaos-hd"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd"}`,
	}

	output := applyAndCompact(t, input)
	// Only the last JOIN needs to remain.
	mustMatchStrings(t, input, output, want)
}

func TestCompactDoubleJoinMultiple(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd,#foobar"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "PART #chaos-hd"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd,#foobar"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "PART #chaos-hd"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd"}`,
	}
	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)

	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	input = []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE2"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd,#foobar"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "PART #chaos-hd"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "PART #foobar"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd"}`,
	}
	want = []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE2"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd"}`,
	}
	output = applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)

	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	input = []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE3"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd,#foobar"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "PART #foobar,#chaos-hd"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd"}`,
	}
	want = []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE3"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chaos-hd"}`,
	}
	output = applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactUser(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :bleh"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
	}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactInvalid(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "USER"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 1}, "Type": 2, "Data": "PART"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 1}, "Type": 2, "Data": "TOPIC"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
	}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactSessionDelete(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 1, "Data": "bye"}`,
	}
	want := []string{}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactSessionDeleteKill(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Operators = append(ircServer.Config.Operators, config.IRCOp{
		Name:     "foo",
		Password: "bar",
	})

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "OPER foo bar"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "KILL secure :bye"}`,
	}
	want := []string{}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactSessionDeleteUserFirst(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 1, "Data": "bye"}`,
	}
	want := []string{}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactSessionDeleteUserMode(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "MODE sECuRE +i"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 1, "Data": "bye"}`,
	}
	want := []string{}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactSessionDeleteAway(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "AWAY :afk"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 1, "Data": "bye"}`,
	}
	want := []string{}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactSessionDeleteJoin(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chan"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 1, "Data": "bye"}`,
	}
	want := []string{}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactSessionDeleteJoinOper(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Operators = append(ircServer.Config.Operators, config.IRCOp{
		Name:     "foo",
		Password: "bar",
	})

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chan"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "OPER foo bar"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 1, "Data": "bye"}`,
	}
	want := []string{}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactSessionDeleteJoinMultiple(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chan,#chan2"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 1, "Data": "bye"}`,
	}
	want := []string{}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactSessionDeleteJoinMultiplePartOne(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chan,#chan2"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "PART #chan"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 1, "Data": "bye"}`,
	}
	want := []string{}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactSessionDeleteJoinMode(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chan"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "MODE #chan"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 1, "Data": "bye"}`,
	}
	want := []string{}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactSessionDeleteJoinModeBans(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chan"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "MODE #chan"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "MODE #chan +b"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 1}, "Type": 2, "Data": "MODE #chan b"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 1}, "Type": 1, "Data": "bye"}`,
	}
	want := []string{}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactSessionDeleteJoinNotFirst(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chan"}`,
		`{"Id": {"Id": 10}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 11}, "Session": {"Id": 10}, "Type": 2, "Data": "NICK mero"}`,
		`{"Id": {"Id": 12}, "Session": {"Id": 10}, "Type": 2, "Data": "USER blah 0 * :Axel Wagner"}`,
		`{"Id": {"Id": 14}, "Session": {"Id": 10}, "Type": 1, "Data": "bye"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chan"}`,
	}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactSessionDeleteJoinModeTopic(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #chan"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "MODE #chan"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "TOPIC #chan"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 1}, "Type": 1, "Data": "bye"}`,
	}
	want := []string{}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactSessionDeletePass(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "PASS bleh"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 1, "Data": "bye"}`,
	}
	want := []string{}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactSessionQuit(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "QUIT foo"}`,
	}
	want := []string{}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactSessionQuitAndDelete(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "QUIT foo"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 1, "Data": "bye"}`,
	}
	want := []string{}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactMessageOfDeath(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 5, "Data": "auth"}`,
	}
	want := []string{}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactNickInUse(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": "USER sECuRE sECuRE localhost :Michael Stapelberg"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": "NICK sECuRE_"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": "JOIN #test"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": "USER sECuRE sECuRE localhost :Michael Stapelberg"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": "NICK sECuRE_"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": "JOIN #test"}`,
	}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactSessionDeleteInvite(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": "NICK mero"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": "USER mero mero localhost :Axel Wagner"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": "INVITE secure #test"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 10}, "Session": {"Id": 1}, "Type": 1, "Data": "bye"}`,
	}
	want := []string{
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": "NICK mero"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": "USER mero mero localhost :Axel Wagner"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": "JOIN #test"}`,
	}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactKeepInvite(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": "NICK mero"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": "USER mero mero localhost :Axel Wagner"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": "JOIN #test_keep"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": "INVITE secure #test_keep"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test_keep"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": "NICK mero"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": "USER mero mero localhost :Axel Wagner"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": "JOIN #test_keep"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": "INVITE secure #test_keep"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test_keep"}`,
	}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactKeepInviteBoth(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": "NICK mero"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": "USER mero mero localhost :Axel Wagner"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": "JOIN #test_keep2"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": "INVITE secure #test_keep2"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 4}, "Type": 1, "Data": "bye"}`,
		`{"Id": {"Id": 10}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test_keep2"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": "NICK mero"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": "USER mero mero localhost :Axel Wagner"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": "JOIN #test_keep2"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": "INVITE secure #test_keep2"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 4}, "Type": 1, "Data": "bye"}`,
		`{"Id": {"Id": 10}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test_keep2"}`,
	}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactInvalidCommands(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "BLAH foo"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
	}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactNickServices(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})

	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK mero"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER mero mero mero :mero"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": "PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 1 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": "NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": "NICK OperServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Operator Server"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 4}, "Type": 2, "Data": "NICK BotServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Bot Server"}`,
		`{"Id": {"Id": 10}, "Session": {"Id": 4}, "Type": 2, "Data": ":ChanServ PRIVMSG sECuRE :foobar"}`,
		`{"Id": {"Id": 11}, "Session": {"Id": 4}, "Type": 2, "Data": ":ChanServ PRIVMSG mero :foobar"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK mero"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER mero mero mero :mero"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": "PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 1 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": "NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": "NICK OperServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Operator Server"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 4}, "Type": 2, "Data": "NICK BotServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Bot Server"}`,
	}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactServerDelete(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": ":services.robustirc.net NICK OperServ 1 1425580295 services services.robustirc.net services.robustirc.net 0 :Operator Service"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 1, "Data": "bye"}`,
	}
	want := []string{}
	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactServerJoinPart(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": ":services.robustirc.net NICK OperServ 1 1425580295 services services.robustirc.net services.robustirc.net 0 :Operator Service"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": ":services.robustirc.net NICK NickServ 1 1431356084 services services.robustirc.net services.robustirc.net 0 :Nickname Registration Service"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": ":OperServ JOIN #test"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 1}, "Type": 2, "Data": ":NickServ JOIN #test"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 1}, "Type": 2, "Data": ":NickServ PART #test"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": ":services.robustirc.net NICK OperServ 1 1425580295 services services.robustirc.net services.robustirc.net 0 :Operator Service"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": ":services.robustirc.net NICK NickServ 1 1431356084 services services.robustirc.net services.robustirc.net 0 :Nickname Registration Service"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": ":OperServ JOIN #test"}`,
	}
	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactServerDeleteInvite(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": ":ChanServ JOIN #test"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 4}, "Type": 2, "Data": ":ChanServ INVITE sECuRE #test"}`,
		`{"Id": {"Id": 10}, "Session": {"Id": 4}, "Type": 1, "Data": "bye"}`,
		`{"Id": {"Id": 11}, "Session": {"Id": 1}, "Type": 1, "Data": "bye"}`,
	}
	want := []string{}
	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactServerSvsjoin(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": ":ChanServ JOIN #test"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 4}, "Type": 2, "Data": "SVSJOIN secure #test"}`,
		`{"Id": {"Id": 10}, "Session": {"Id": 1}, "Type": 2, "Data": "PART #test"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": ":ChanServ JOIN #test"}`,
	}
	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactServerSvspart(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": ":ChanServ JOIN #test"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 4}, "Type": 2, "Data": "SVSJOIN secure #test"}`,
		`{"Id": {"Id": 10}, "Session": {"Id": 4}, "Type": 2, "Data": "SVSPART secure #test"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": ":ChanServ JOIN #test"}`,
	}
	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactServerDeleteSvsjoin(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": "SVSJOIN secure #test"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 4}, "Type": 1, "Data": "bye"}`,
		`{"Id": {"Id": 10}, "Session": {"Id": 1}, "Type": 1, "Data": "bye"}`,
	}
	want := []string{}
	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactServerKeepSvsjoin(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": ":ChanServ JOIN #test"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 4}, "Type": 2, "Data": "SVSJOIN secure #test"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": ":ChanServ JOIN #test"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 4}, "Type": 2, "Data": "SVSJOIN secure #test"}`,
	}
	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactServerModes(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 4}, "Type": 2, "Data": ":ChanServ MODE #test +i"}`,
		`{"Id": {"Id": 10}, "Session": {"Id": 4}, "Type": 2, "Data": ":ChanServ MODE #test -i"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test"}`,
	}
	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactServerKill(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": ":ChanServ KILL sECuRE :We don’t take kindly to your type around here!"}`,
	}
	want := []string{
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
	}
	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactServerKick(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 4}, "Type": 2, "Data": ":ChanServ KICK #test sECuRE :This channel is reserved"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
	}
	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactServerTopic(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 4}, "Type": 2, "Data": ":ChanServ TOPIC #test frank 1438898523 :bleh"}`,
		`{"Id": {"Id": 10}, "Session": {"Id": 1}, "Type": 2, "Data": "TOPIC #test :updated"}`,
	}

	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 10}, "Session": {"Id": 1}, "Type": 2, "Data": "TOPIC #test :updated"}`,
	}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactServerSvsnick(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SVSNICK sECuRE Guest45226 1454250684"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE_bleh"}`,
	}

	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE_bleh"}`,
	}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactDeleteServerSvsnick(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SVSNICK sECuRE Guest45226 1454250684"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 1}, "Type": 1, "Data": "bye"}`,
	}

	want := []string{
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
	}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactServerSvsmode(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK NickServ 1 1431356084 services services.robustirc.net services.robustirc.net 0 :Nickname Registration Service"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": ":NickServ SVSMODE sECuRE +r"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 4}, "Type": 2, "Data": ":NickServ SVSMODE sECuRE -r"}`,
		`{"Id": {"Id": 10}, "Session": {"Id": 4}, "Type": 2, "Data": ":NickServ SVSMODE sECuRE +r"}`,
	}

	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK NickServ 1 1431356084 services services.robustirc.net services.robustirc.net 0 :Nickname Registration Service"}`,
		`{"Id": {"Id": 10}, "Session": {"Id": 4}, "Type": 2, "Data": ":NickServ SVSMODE sECuRE +r"}`,
	}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactServerDeleteSvsmode(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK NickServ 1 1431356084 services services.robustirc.net services.robustirc.net 0 :Nickname Registration Service"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": ":NickServ SVSMODE sECuRE +r"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 1}, "Type": 1, "Data": "bye"}`,
	}

	want := []string{
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK NickServ 1 1431356084 services services.robustirc.net services.robustirc.net 0 :Nickname Registration Service"}`,
	}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactServerSvshold(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK NickServ 1 1431356084 services services.robustirc.net services.robustirc.net 0 :Nickname Registration Service"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": ":NickServ SVSHOLD Merovius 60 :Being held for registered user"}`,
		`{"Id": {"Id": 81}, "Session": {"Id": 4}, "Type": 2, "Data": ":NickServ JOIN #test"}`,
	}

	want := []string{
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK NickServ 1 1431356084 services services.robustirc.net services.robustirc.net 0 :Nickname Registration Service"}`,
		`{"Id": {"Id": 81}, "Session": {"Id": 4}, "Type": 2, "Data": ":NickServ JOIN #test"}`,
	}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestKeepServerSvshold(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK NickServ 1 1431356084 services services.robustirc.net services.robustirc.net 0 :Nickname Registration Service"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": ":NickServ SVSHOLD Merovius 60 :Being held for registered user"}`,
	}

	want := []string{
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK NickServ 1 1431356084 services services.robustirc.net services.robustirc.net 0 :Nickname Registration Service"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": ":NickServ SVSHOLD Merovius 60 :Being held for registered user"}`,
	}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestKeepServerQuit(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK NickServ 1 1431356084 services services.robustirc.net services.robustirc.net 0 :Nickname Registration Service"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 4}, "Type": 2, "Data": ":NickServ QUIT :taking a break"}`,
	}

	want := []string{
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK NickServ 1 1431356084 services services.robustirc.net services.robustirc.net 0 :Nickname Registration Service"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 4}, "Type": 2, "Data": ":NickServ QUIT :taking a break"}`,
	}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactServerQuit(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK NickServ 1 1431356084 services services.robustirc.net services.robustirc.net 0 :Nickname Registration Service"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 4}, "Type": 2, "Data": ":NickServ QUIT :taking a break"}`,
		`{"Id": {"Id": 10}, "Session": {"Id": 4}, "Type": 1, "Data": "bye"}`,
	}

	want := []string{}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactServerQuitMultiple(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 4}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net PASS :services=mypass"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net SERVER services.robustirc.net 0 :Services for IRC Networks"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK NickServ 1 1431356084 services services.robustirc.net services.robustirc.net 0 :Nickname Registration Service"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 4}, "Type": 2, "Data": ":services.robustirc.net NICK ChanServ 1 1422134861 services robustirc.net services.robustirc.net 0 :Nick Server"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 4}, "Type": 2, "Data": ":NickServ QUIT :taking a break"}`,
		`{"Id": {"Id": 10}, "Session": {"Id": 4}, "Type": 2, "Data": ":ChanServ QUIT :taking a break"}`,
		`{"Id": {"Id": 11}, "Session": {"Id": 4}, "Type": 1, "Data": "bye"}`,
	}

	want := []string{}

	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactModeCancellation(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 5}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 5}, "Type": 2, "Data": "NICK mero"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 5}, "Type": 2, "Data": "USER blah 0 * :Axel Wagner"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 5}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 1}, "Type": 2, "Data": "MODE #test +o mero"}`,
		`{"Id": {"Id": 10}, "Session": {"Id": 1}, "Type": 2, "Data": "MODE #test +i"}`,
		`{"Id": {"Id": 11}, "Session": {"Id": 5}, "Type": 2, "Data": "MODE #test -i"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 5}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 5}, "Type": 2, "Data": "NICK mero"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 5}, "Type": 2, "Data": "USER blah 0 * :Axel Wagner"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 5}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 1}, "Type": 2, "Data": "MODE #test +o mero"}`,
	}
	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

// TODO(secure): enable this test once channel keys are implemented
/*
func TestCompactModeObsolete(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "MODE #test +k foo"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "MODE #test +k bar"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "MODE #test +k bar"}`,
	}
	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}
*/

func TestKeepPartialMode(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "MODE #test +int"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "MODE #test -i"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "MODE #test +int"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "MODE #test -i"}`,
	}
	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactPartialMode(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "MODE #test +int"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "MODE #test -i"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 1}, "Type": 2, "Data": "MODE #test -nt"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test"}`,
	}
	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactKick(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 5}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 5}, "Type": 2, "Data": "NICK mero"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 5}, "Type": 2, "Data": "USER blah 0 * :Axel Wagner"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 5}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 1}, "Type": 2, "Data": "KICK #test mero"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 5}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 5}, "Type": 2, "Data": "NICK mero"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 5}, "Type": 2, "Data": "USER blah 0 * :Axel Wagner"}`,
	}
	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestKeepKickPartial(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 5}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 5}, "Type": 2, "Data": "NICK mero"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 5}, "Type": 2, "Data": "USER blah 0 * :Axel Wagner"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 5}, "Type": 2, "Data": "JOIN #test,#more"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 1}, "Type": 2, "Data": "KICK #test mero"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 5}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 5}, "Type": 2, "Data": "NICK mero"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 5}, "Type": 2, "Data": "USER blah 0 * :Axel Wagner"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 5}, "Type": 2, "Data": "JOIN #test,#more"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 1}, "Type": 2, "Data": "KICK #test mero"}`,
	}
	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestKeepChanModePartial(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 5}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 5}, "Type": 2, "Data": "NICK mero"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 5}, "Type": 2, "Data": "USER blah 0 * :Axel Wagner"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 5}, "Type": 2, "Data": "JOIN #test,#more"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 1}, "Type": 2, "Data": "KICK #test mero"}`,
		`{"Id": {"Id": 10}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #more"}`,
		`{"Id": {"Id": 11}, "Session": {"Id": 5}, "Type": 2, "Data": "MODE #more +o secure"}`,
		`{"Id": {"Id": 12}, "Session": {"Id": 1}, "Type": 2, "Data": "KICK #more mero"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test"}`,
		`{"Id": {"Id": 5}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 5}, "Type": 2, "Data": "NICK mero"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 5}, "Type": 2, "Data": "USER blah 0 * :Axel Wagner"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 5}, "Type": 2, "Data": "JOIN #test,#more"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 1}, "Type": 2, "Data": "KICK #test mero"}`,
		`{"Id": {"Id": 10}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #more"}`,
		`{"Id": {"Id": 11}, "Session": {"Id": 5}, "Type": 2, "Data": "MODE #more +o secure"}`,
		`{"Id": {"Id": 12}, "Session": {"Id": 1}, "Type": 2, "Data": "KICK #more mero"}`,
	}
	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactKickPartial(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	ircServer.Config.Services = append(ircServer.Config.Services, config.Service{
		Password: "mypass",
	})
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test,#more"}`,
		`{"Id": {"Id": 5}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 5}, "Type": 2, "Data": "NICK mero"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 5}, "Type": 2, "Data": "USER blah 0 * :Axel Wagner"}`,
		`{"Id": {"Id": 8}, "Session": {"Id": 5}, "Type": 2, "Data": "JOIN #test,#more"}`,
		`{"Id": {"Id": 9}, "Session": {"Id": 1}, "Type": 2, "Data": "KICK #test mero"}`,
		`{"Id": {"Id": 10}, "Session": {"Id": 1}, "Type": 2, "Data": "KICK #more mero"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "JOIN #test,#more"}`,
		`{"Id": {"Id": 5}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 5}, "Type": 2, "Data": "NICK mero"}`,
		`{"Id": {"Id": 7}, "Session": {"Id": 5}, "Type": 2, "Data": "USER blah 0 * :Axel Wagner"}`,
	}
	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestCompactAway(t *testing.T) {
	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	input := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "AWAY :foo"}`,
	}
	want := []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "AWAY :foo"}`,
	}
	output := applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)

	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	input = []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE2"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "AWAY :foo"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "AWAY :"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "AWAY :bar"}`,
	}
	want = []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE2"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 6}, "Session": {"Id": 1}, "Type": 2, "Data": "AWAY :bar"}`,
	}
	output = applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)

	ircServer = ircserver.NewIRCServer("", "testnetwork", time.Now())
	input = []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE3"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
		`{"Id": {"Id": 4}, "Session": {"Id": 1}, "Type": 2, "Data": "AWAY :foo"}`,
		`{"Id": {"Id": 5}, "Session": {"Id": 1}, "Type": 2, "Data": "AWAY :"}`,
	}
	want = []string{
		`{"Id": {"Id": 1}, "Type": 0, "Data": "auth"}`,
		`{"Id": {"Id": 2}, "Session": {"Id": 1}, "Type": 2, "Data": "NICK sECuRE3"}`,
		`{"Id": {"Id": 3}, "Session": {"Id": 1}, "Type": 2, "Data": "USER blah 0 * :Michael Stapelberg"}`,
	}
	output = applyAndCompact(t, input)
	mustMatchStrings(t, input, output, want)
}

func TestMain(m *testing.M) {
	defer glog.Flush()
	flag.Parse()
	tempdir, err := ioutil.TempDir("", "robustirc-test-raftdir-")
	if err != nil {
		log.Fatal(err)
	}
	raftDir = &tempdir
	// TODO: cleanup tmp-outputstream and permanent-compaction*
	os.Exit(m.Run())
}
