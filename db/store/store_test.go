package store

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	sql "github.com/caesium-cloud/caesium/db"
	"github.com/caesium-cloud/caesium/db/command"
	"github.com/caesium-cloud/caesium/db/testdata/chinook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
)

type StoreTestSuite struct {
	suite.Suite
	m sync.Map
}

func (s *StoreTestSuite) SetupTest() {
	store := mustNewStore(strings.Contains(s.T().Name(), "Memory"))
	assert.Nil(s.T(), store.Open(true))
	_, err := store.WaitForLeader(10 * time.Second)
	assert.Nil(s.T(), err)
	s.m.Store(s.T().Name(), store)
}

func (s *StoreTestSuite) TeardownTest() {
	assert.Nil(s.T(), s.Store().Close(true))
	assert.Nil(s.T(), os.RemoveAll(s.Store().Path()))
	s.m.Delete(s.T().Name())
}

func (s *StoreTestSuite) Store() *Store {
	v, ok := s.m.Load(s.T().Name())
	assert.True(s.T(), ok)
	return v.(*Store)
}

func (s *StoreTestSuite) TestOpenSingleNode() {
	got, exp := s.Store().LeaderAddr(), s.Store().Addr()
	assert.Equal(s.T(), exp, got)

	id, err := s.Store().LeaderID()
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), id, s.Store().raftID)
}

func (s *StoreTestSuite) TestSingleNodeInMemoryQuery() {
	eReq := executeRequestFromStrings([]string{
		`CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)`,
		`INSERT INTO foo(id, name) VALUES(1, "bar")`,
	}, false, false)
	_, err := s.Store().Execute(eReq)
	assert.Nil(s.T(), err)

	qReq := queryRequestFromString("SELECT * FROM foo", false, false)
	qReq.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_NONE
	r, err := s.Store().Query(qReq)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["id","name"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[1,"bar"]]`, asJSON(r[0].Values))
}

func (s *StoreTestSuite) TestSingleNodeInMemoryQueryFail() {
	eReq := executeRequestFromStrings([]string{
		`INSERT INTO foo(id, name) VALUES(1, "bar")`,
	}, false, false)
	r, err := s.Store().Execute(eReq)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), "no such table: foo", r[0].Error)
}
func (s *StoreTestSuite) TestSingleNodeFileQuery() {
	eReq := executeRequestFromStrings([]string{
		`CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)`,
		`INSERT INTO foo(id, name) VALUES(1, "bar")`,
	}, false, false)
	_, err := s.Store().Execute(eReq)
	assert.Nil(s.T(), err)

	qReq := queryRequestFromString("SELECT * FROM foo", false, false)
	qReq.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_NONE
	r, err := s.Store().Query(qReq)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["id","name"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[1,"bar"]]`, asJSON(r[0].Values))

	qReq = queryRequestFromString("SELECT * FROM foo", false, false)
	qReq.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_WEAK
	r, err = s.Store().Query(qReq)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["id","name"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[1,"bar"]]`, asJSON(r[0].Values))

	qReq = queryRequestFromString("SELECT * FROM foo", false, false)
	qReq.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_STRONG
	r, err = s.Store().Query(qReq)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["id","name"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[1,"bar"]]`, asJSON(r[0].Values))

	qReq = queryRequestFromString("SELECT * FROM foo", false, true)
	qReq.Timings = true
	qReq.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_NONE
	r, err = s.Store().Query(qReq)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["id","name"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[1,"bar"]]`, asJSON(r[0].Values))

	qReq = queryRequestFromString("SELECT * FROM foo", true, false)
	qReq.Request.Transaction = true
	qReq.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_NONE
	r, err = s.Store().Query(qReq)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["id","name"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[1,"bar"]]`, asJSON(r[0].Values))
}

func (s *StoreTestSuite) TestSingleNodeQueryTransaction() {
	eReq := executeRequestFromStrings([]string{
		`CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)`,
		`INSERT INTO foo(id, name) VALUES(1, "bar")`,
	}, false, true)
	_, err := s.Store().Execute(eReq)
	assert.Nil(s.T(), err)

	qReq := queryRequestFromString("SELECT * FROM foo", false, true)
	var r []*sql.Rows

	qReq.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_NONE
	_, err = s.Store().Query(qReq)
	assert.Nil(s.T(), err)

	qReq.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_WEAK
	_, err = s.Store().Query(qReq)
	assert.Nil(s.T(), err)

	qReq.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_STRONG
	r, err = s.Store().Query(qReq)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["id","name"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[1,"bar"]]`, asJSON(r[0].Values))
}

func (s *StoreTestSuite) TestSingleNodeBackupBinary() {
	dump := `PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
CREATE TABLE foo (id integer not null primary key, name text);
INSERT INTO "foo" VALUES(1,'bar');
COMMIT;
`
	_, err := s.Store().Execute(executeRequestFromString(dump, false, false))
	assert.Nil(s.T(), err)

	f, err := ioutil.TempFile("", "caesium-baktest-")
	assert.Nil(s.T(), err)
	defer os.Remove(f.Name())

	assert.Nil(s.T(), s.Store().Backup(true, BackupBinary, f))

	// Check the backed up data by reading back up file, underlying SQLite file,
	// and comparing the two.
	bkp, err := ioutil.ReadFile(f.Name())
	assert.Nil(s.T(), err)

	dbFile, err := ioutil.ReadFile(filepath.Join(s.Store().Path(), sqliteFile))
	assert.Nil(s.T(), err)
	assert.True(s.T(), bytes.Equal(bkp, dbFile))
}

func (s *StoreTestSuite) TestSingleNodeBackupText() {
	dump := `PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
CREATE TABLE foo (id integer not null primary key, name text);
INSERT INTO "foo" VALUES(1,'bar');
COMMIT;
`
	_, err := s.Store().Execute(executeRequestFromString(dump, false, false))
	assert.Nil(s.T(), err)

	f, err := ioutil.TempFile("", "caesium-baktest-")
	assert.Nil(s.T(), err)
	defer os.Remove(f.Name())

	assert.Nil(s.T(), s.Store().Backup(true, BackupSQL, f))

	// Check the backed up data
	bkp, err := ioutil.ReadFile(f.Name())
	assert.Nil(s.T(), err)
	assert.True(s.T(), bytes.Equal(bkp, []byte(dump)))
}

func (s *StoreTestSuite) TestSingleNodeLoad() {
	dump := `PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
CREATE TABLE foo (id integer not null primary key, name text);
INSERT INTO "foo" VALUES(1,'bar');
COMMIT;
`
	_, err := s.Store().Execute(executeRequestFromString(dump, false, false))
	assert.Nil(s.T(), err)

	// Check that data were loaded correctly.
	qr := queryRequestFromString("SELECT * FROM foo", false, true)
	qr.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_STRONG
	r, err := s.Store().Query(qr)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["id","name"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[1,"bar"]]`, asJSON(r[0].Values))
}

func (s *StoreTestSuite) TestSingleNodeSingleCmdTrigger() {
	dump := `PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
CREATE TABLE foo (id integer primary key asc, name text);
INSERT INTO "foo" VALUES(1,'bob');
INSERT INTO "foo" VALUES(2,'alice');
INSERT INTO "foo" VALUES(3,'eve');
CREATE TABLE bar (nameid integer, age integer);
INSERT INTO "bar" VALUES(1,44);
INSERT INTO "bar" VALUES(2,46);
INSERT INTO "bar" VALUES(3,8);
CREATE VIEW foobar as select name as Person, Age as age from foo inner join bar on foo.id == bar.nameid;
CREATE TRIGGER new_foobar instead of insert on foobar begin insert into foo (name) values (new.Person); insert into bar (nameid, age) values ((select id from foo where name == new.Person), new.Age); end;
COMMIT;
`
	_, err := s.Store().Execute(executeRequestFromString(dump, false, false))
	assert.Nil(s.T(), err)

	// Check that the VIEW and TRIGGER are OK by using both.
	er := executeRequestFromString("INSERT INTO foobar VALUES('jason', 16)", false, true)
	r, err := s.Store().Execute(er)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), int64(3), r[0].LastInsertID)
}

func (s *StoreTestSuite) TestSingleNodeLoadNoStatement() {
	dump := `PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
COMMIT;
`
	_, err := s.Store().Execute(executeRequestFromString(dump, false, false))
	assert.Nil(s.T(), err)
}

func (s *StoreTestSuite) TestSingleNodeLoadEmpty() {
	dump := ``
	_, err := s.Store().Execute(executeRequestFromString(dump, false, false))
	assert.Nil(s.T(), err)
}

func (s *StoreTestSuite) TestSingleNodeLoadAbortOnError() {
	dump := `PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT);
COMMIT;`

	r, err := s.Store().Execute(executeRequestFromString(dump, false, false))
	assert.Nil(s.T(), err)
	assert.Empty(s.T(), r[0].Error)

	r, err = s.Store().Execute(executeRequestFromString(dump, false, false))
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), "table foo already exists", r[0].Error)

	r, err = s.Store().Execute(executeRequestFromString(dump, false, false))
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), "cannot start a transaction within a transaction", r[0].Error)

	r, err = s.Store().ExecuteOrAbort(executeRequestFromString(dump, false, false))
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), "cannot start a transaction within a transaction", r[0].Error)

	r, err = s.Store().Execute(executeRequestFromString(dump, false, false))
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), "table foo already exists", r[0].Error)
}

func (s *StoreTestSuite) TestSingleNodeLoadChinook() {
	_, err := s.Store().Execute(executeRequestFromString(chinook.DB, false, false))
	assert.Nil(s.T(), err)

	qr := queryRequestFromString("SELECT count(*) FROM track", false, true)
	qr.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_STRONG
	r, err := s.Store().Query(qr)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["count(*)"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[3503]]`, asJSON(r[0].Values))

	qr = queryRequestFromString("SELECT count(*) FROM album", false, true)
	qr.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_STRONG
	r, err = s.Store().Query(qr)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["count(*)"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[347]]`, asJSON(r[0].Values))

	qr = queryRequestFromString("SELECT count(*) FROM artist", false, true)
	qr.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_STRONG
	r, err = s.Store().Query(qr)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["count(*)"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[275]]`, asJSON(r[0].Values))
}

func (s *StoreTestSuite) TestMultiNodeJoinRemove() {
	s0 := s.Store()

	s1 := mustNewStore(true)
	defer os.RemoveAll(s1.Path())
	assert.Nil(s.T(), s1.Open(false))
	defer s1.Close(true)

	// Get sorted list of cluster nodes.
	storeNodes := []string{s0.ID(), s1.ID()}
	sort.StringSlice(storeNodes).Sort()

	// Join the second node to the first.
	assert.Nil(s.T(), s0.Join(s1.ID(), s1.Addr(), true, nil))

	s1.WaitForLeader(10 * time.Second)

	assert.Equal(s.T(), s1.LeaderAddr(), s0.Addr())
	assert.True(s.T(), s0.IsLeader())
	assert.Equal(s.T(), Leader, s0.State())
	assert.Equal(s.T(), Follower, s1.State())

	id, err := s1.LeaderID()
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), id, s0.raftID)

	nodes, err := s0.Nodes()
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), len(nodes), len(storeNodes))
	assert.Equal(s.T(), storeNodes[0], nodes[0].ID)
	assert.Equal(s.T(), storeNodes[1], nodes[1].ID)

	// Remove a node.
	assert.Nil(s.T(), s0.Remove(s1.ID()))

	nodes, err = s0.Nodes()
	assert.Nil(s.T(), err)
	assert.Len(s.T(), nodes, 1)
	assert.Equal(s.T(), s0.ID(), nodes[0].ID)
}

func (s *StoreTestSuite) TestMultiNodeJoinNonVoterRemove() {
	s0 := s.Store()

	s1 := mustNewStore(true)
	defer os.RemoveAll(s1.Path())
	assert.Nil(s.T(), s1.Open(false))
	defer s1.Close(true)

	// Get sorted list of cluster nodes.
	storeNodes := []string{s0.ID(), s1.ID()}
	sort.StringSlice(storeNodes).Sort()

	// Join the second node to the first.
	assert.Nil(s.T(), s0.Join(s1.ID(), s1.Addr(), false, nil))

	s1.WaitForLeader(10 * time.Second)

	// Check leader state on follower.
	assert.Equal(s.T(), s1.LeaderAddr(), s0.Addr())

	id, err := s1.LeaderID()
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), id, s0.raftID)

	nodes, err := s0.Nodes()
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), len(nodes), len(storeNodes))
	assert.Equal(s.T(), storeNodes[0], nodes[0].ID)
	assert.Equal(s.T(), storeNodes[1], nodes[1].ID)

	assert.Nil(s.T(), s0.Remove(s1.ID()))

	nodes, err = s0.Nodes()
	assert.Nil(s.T(), err)
	assert.Len(s.T(), nodes, 1)
	assert.Equal(s.T(), s0.ID(), nodes[0].ID)
}

func (s *StoreTestSuite) TestMultiNodeExecuteQuery() {
	s0 := s.Store()

	s1 := mustNewStore(true)
	defer os.RemoveAll(s1.Path())
	assert.Nil(s.T(), s1.Open(false))
	defer s1.Close(true)

	s2 := mustNewStore(true)
	defer os.RemoveAll(s2.Path())
	assert.Nil(s.T(), s2.Open(false))
	defer s2.Close(true)

	// Join the second node to the first as a voting node.
	assert.Nil(s.T(), s0.Join(s1.ID(), s1.Addr(), true, nil))

	// Join the third node to the first as a non-voting node.
	assert.Nil(s.T(), s0.Join(s2.ID(), s2.Addr(), false, nil))

	er := executeRequestFromStrings([]string{
		`CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)`,
		`INSERT INTO foo(id, name) VALUES(1, "bar")`,
	}, false, false)
	_, err := s0.Execute(er)
	assert.Nil(s.T(), err)

	qr := queryRequestFromString("SELECT * FROM foo", false, false)
	qr.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_NONE
	r, err := s0.Query(qr)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["id","name"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[1,"bar"]]`, asJSON(r[0].Values))

	// Wait until the 3 log entries have been applied to the voting follower,
	// and then query.
	assert.Nil(s.T(), s1.WaitForAppliedIndex(3, 5*time.Second))

	qr.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_WEAK
	_, err = s1.Query(qr)
	assert.NotNil(s.T(), err)

	qr.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_STRONG
	_, err = s1.Query(qr)
	assert.NotNil(s.T(), err)

	qr.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_NONE
	r, err = s1.Query(qr)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["id","name"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[1,"bar"]]`, asJSON(r[0].Values))

	// Wait until the 3 log entries have been applied to the non-voting follower,
	// and then query.
	assert.Nil(s.T(), s2.WaitForAppliedIndex(3, 5*time.Second))

	qr.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_WEAK
	_, err = s1.Query(qr)
	assert.NotNil(s.T(), err)

	qr.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_STRONG
	_, err = s1.Query(qr)
	assert.NotNil(s.T(), err)

	qr.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_NONE
	r, err = s1.Query(qr)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["id","name"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[1,"bar"]]`, asJSON(r[0].Values))
}

func (s *StoreTestSuite) TestMultiNodeExecuteQueryFreshness() {
	s0 := s.Store()

	s1 := mustNewStore(true)
	defer os.RemoveAll(s1.Path())
	assert.Nil(s.T(), s1.Open(false))
	defer s1.Close(true)

	// Join the second node to the first.
	assert.Nil(s.T(), s0.Join(s1.ID(), s1.Addr(), true, nil))

	er := executeRequestFromStrings([]string{
		`CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)`,
		`INSERT INTO foo(id, name) VALUES(1, "bar")`,
	}, false, false)
	_, err := s0.Execute(er)
	assert.Nil(s.T(), err)

	qr := queryRequestFromString("SELECT * FROM foo", false, false)
	qr.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_NONE
	r, err := s0.Query(qr)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["id","name"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[1,"bar"]]`, asJSON(r[0].Values))

	// Wait until the 3 log entries have been applied to the follower,
	// and then query.
	assert.Nil(s.T(), s1.WaitForAppliedIndex(3, 5*time.Second))

	// "Weak" consistency queries with 1 nanosecond freshness should pass, because freshness
	// is ignored in this case.
	qr.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_WEAK
	qr.Freshness = mustParseDuration("1ns").Nanoseconds()
	_, err = s0.Query(qr)
	assert.Nil(s.T(), err)

	qr.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_STRONG
	// "Strong" consistency queries with 1 nanosecond freshness should pass, because freshness
	// is ignored in this case.
	_, err = s0.Query(qr)
	assert.Nil(s.T(), err)

	// Kill leader.
	assert.Nil(s.T(), s0.Close(true))

	// "None" consistency queries should still work.
	qr.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_NONE
	qr.Freshness = 0
	r, err = s1.Query(qr)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["id","name"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[1,"bar"]]`, asJSON(r[0].Values))

	// Wait for the freshness interval to pass.
	time.Sleep(mustParseDuration("1s"))

	// "None" consistency queries with 1 nanosecond freshness should fail, because at least
	// one nanosecond *should* have passed since leader died (surely!).
	qr.Level = command.QueryRequest_QUERY_REQUEST_LEVEL_NONE
	qr.Freshness = mustParseDuration("1ns").Nanoseconds()
	_, err = s1.Query(qr)
	assert.Equal(s.T(), ErrStaleRead, err)

	// Freshness of 0 is ignored.
	qr.Freshness = 0
	r, err = s1.Query(qr)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["id","name"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[1,"bar"]]`, asJSON(r[0].Values))

	// "None" consistency queries with 1 hour freshness should pass, because it should
	// not be that long since the leader died.
	qr.Freshness = mustParseDuration("1h").Nanoseconds()
	r, err = s1.Query(qr)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["id","name"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[1,"bar"]]`, asJSON(r[0].Values))
}

func (s *StoreTestSuite) TestLogTruncationMultiNode() {
	s0 := s.Store()
	s0.Close(true)

	s0.SnapshotThreshold = 4
	s0.SnapshotInterval = 100 * time.Millisecond

	assert.Nil(s.T(), s0.Open(true))
	s0.WaitForLeader(10 * time.Second)
	nSnaps := stats.Get(numSnaphots).String()

	// Write more than s.SnapshotThreshold statements.
	queries := []string{
		`CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)`,
		`INSERT INTO foo(id, name) VALUES(1, "bar")`,
		`INSERT INTO foo(id, name) VALUES(2, "bar")`,
		`INSERT INTO foo(id, name) VALUES(3, "bar")`,
		`INSERT INTO foo(id, name) VALUES(4, "bar")`,
		`INSERT INTO foo(id, name) VALUES(5, "bar")`,
	}
	for i := range queries {
		_, err := s0.Execute(executeRequestFromString(queries[i], false, false))
		assert.Nil(s.T(), err)
	}

	// Wait for the snapshot to happen and log to be truncated.
	f := func() bool {
		return stats.Get(numSnaphots).String() != nSnaps
	}
	testPoll(s.T(), f, 100*time.Millisecond, 2*time.Second)

	// Fire up new node and ensure it picks up all changes. This will
	// involve getting a snapshot and truncated log.
	s1 := mustNewStore(true)
	assert.Nil(s.T(), s1.Open(true))
	defer s1.Close(true)

	// Join the second node to the first.
	assert.Nil(s.T(), s0.Join(s1.ID(), s1.Addr(), true, nil))
	s1.WaitForLeader(10 * time.Second)

	// Wait until the log entries have been applied to the follower,
	// and then query.
	assert.Nil(s.T(), s1.WaitForAppliedIndex(8, 5*time.Second))

	qr := queryRequestFromString("SELECT count(*) FROM foo", false, true)
	r, err := s1.Query(qr)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["count(*)"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[5]]`, asJSON(r[0].Values))
}

func (s *StoreTestSuite) TestSingleNodeSnapshotOnDisk() {
	queries := []string{
		`CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)`,
		`INSERT INTO foo(id, name) VALUES(1, "bar")`,
	}
	_, err := s.Store().Execute(executeRequestFromStrings(queries, false, false))
	assert.Nil(s.T(), err)

	_, err = s.Store().Query(queryRequestFromString("SELECT * FROM foo", false, false))
	assert.Nil(s.T(), err)

	// Snap the node and write to disk.
	f, err := s.Store().Snapshot()
	assert.Nil(s.T(), err)

	snapDir := mustTempDir()
	defer os.RemoveAll(snapDir)
	snapFile, err := os.Create(filepath.Join(snapDir, "snapshot"))
	assert.Nil(s.T(), err)
	assert.Nil(s.T(), f.Persist(&mockSnapshotSink{snapFile}))

	// Check restoration.
	snapFile, err = os.Open(filepath.Join(snapDir, "snapshot"))
	assert.Nil(s.T(), err)
	assert.Nil(s.T(), s.Store().Restore(snapFile))

	// Ensure database is back in the correct state.
	r, err := s.Store().Query(queryRequestFromString("SELECT * FROM foo", false, false))
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["id","name"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[1,"bar"]]`, asJSON(r[0].Values))
}

func (s *StoreTestSuite) TestSingleNodeSnapshotInMemory() {
	queries := []string{
		`CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)`,
		`INSERT INTO foo(id, name) VALUES(1, "bar")`,
	}

	_, err := s.Store().Execute(executeRequestFromStrings(queries, false, false))
	assert.Nil(s.T(), err)

	_, err = s.Store().Query(queryRequestFromString("SELECT * FROM foo", false, false))
	assert.Nil(s.T(), err)

	// Snap the node and write to disk.
	f, err := s.Store().Snapshot()
	assert.Nil(s.T(), err)

	snapDir := mustTempDir()
	defer os.RemoveAll(snapDir)
	snapFile, err := os.Create(filepath.Join(snapDir, "snapshot"))
	assert.Nil(s.T(), err)

	assert.Nil(s.T(), f.Persist(&mockSnapshotSink{snapFile}))

	// Check restoration.
	snapFile, err = os.Open(filepath.Join(snapDir, "snapshot"))
	assert.Nil(s.T(), err)

	assert.Nil(s.T(), s.Store().Restore(snapFile))

	// Ensure database is back in the correct state.
	r, err := s.Store().Query(queryRequestFromString("SELECT * FROM foo", false, false))
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["id","name"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[1,"bar"]]`, asJSON(r[0].Values))

	// Write a record, ensuring it works and can be queried back i.e. that
	// the system remains functional.
	_, err = s.Store().Execute(executeRequestFromString(`INSERT INTO foo(id, name) VALUES(2, "bar")`, false, false))
	assert.Nil(s.T(), err)

	r, err = s.Store().Query(queryRequestFromString("SELECT COUNT(*) FROM foo", false, false))
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["COUNT(*)"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[2]]`, asJSON(r[0].Values))
}

func (s *StoreTestSuite) TestSingleNodeRestoreUncompressed() {
	// Check restoration from a pre-compressed SQLite database snap.
	// This is to test for backwards compatilibty of this code.
	f, err := os.Open(filepath.Join("testdata", "uncompressed-sqlite-snap.bin"))
	assert.Nil(s.T(), err)
	assert.Nil(s.T(), s.Store().Restore(f))

	// Ensure database is back in the expected state.
	r, err := s.Store().Query(queryRequestFromString("SELECT count(*) FROM foo", false, false))
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `["count(*)"]`, asJSON(r[0].Columns))
	assert.Equal(s.T(), `[[5000]]`, asJSON(r[0].Values))
}

func (s *StoreTestSuite) TestSingleNodeNoop() {
	assert.Nil(s.T(), s.Store().Noop("1"))
	assert.Equal(s.T(), s.Store().numNoops, 1)
}

func (s *StoreTestSuite) TestMetadataMultiNode() {
	s0 := s.Store()

	s1 := mustNewStore(true)
	assert.Nil(s.T(), s1.Open(true))
	defer s1.Close(true)
	s1.WaitForLeader(10 * time.Second)

	assert.Empty(s.T(), s0.Metadata(s0.raftID, "foo"))
	assert.Empty(s.T(), s0.Metadata("nonsense", "foo"))

	assert.Nil(s.T(), s0.SetMetadata(map[string]string{"foo": "bar"}))
	assert.Equal(s.T(), s0.Metadata(s0.raftID, "foo"), "bar")
	assert.Empty(s.T(), s0.Metadata("nonsense", "foo"))

	// Join the second node to the first.
	meta := map[string]string{"baz": "qux"}
	assert.Nil(s.T(), s0.Join(s1.ID(), s1.Addr(), true, meta))

	s1.WaitForLeader(10 * time.Second)
	// Wait until the log entries have been applied to the follower,
	// and then query.
	assert.Nil(s.T(), s1.WaitForAppliedIndex(5, 5*time.Second))
	assert.Equal(s.T(), s1.Metadata(s0.raftID, "foo"), "bar")
	assert.Equal(s.T(), s0.Metadata(s1.raftID, "baz"), "qux")

	// Remove a node.
	assert.Nil(s.T(), s0.Remove(s1.ID()))
	assert.Equal(s.T(), s1.Metadata(s0.raftID, "foo"), "bar")
	assert.Empty(s.T(), s0.Metadata(s1.raftID, "baz"))
}

func (s *StoreTestSuite) TestStats() {
	stats, err := s.Store().Stats()
	assert.Nil(s.T(), err)
	assert.NotNil(s.T(), stats)
}

func mustNewStoreAtPath(path string, inmem bool) *Store {
	cfg := NewDBConfig("", inmem)
	s := New(mustMockListener("localhost:0"), &StoreConfig{
		DBConf: cfg,
		Dir:    path,
		ID:     path, // Could be any unique string.
	})
	if s == nil {
		panic("failed to create new store")
	}
	return s
}

func mustNewStore(inmem bool) *Store {
	return mustNewStoreAtPath(mustTempDir(), inmem)
}

type mockSnapshotSink struct {
	*os.File
}

func (m *mockSnapshotSink) ID() string {
	return "1"
}

func (m *mockSnapshotSink) Cancel() error {
	return nil
}

type mockTransport struct {
	ln net.Listener
}

type mockListener struct {
	ln net.Listener
}

func mustMockListener(addr string) Listener {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		panic("failed to create new listner")
	}
	return &mockListener{ln}
}

func (m *mockListener) Dial(addr string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("tcp", addr, timeout)
}

func (m *mockListener) Accept() (net.Conn, error) { return m.ln.Accept() }

func (m *mockListener) Close() error { return m.ln.Close() }

func (m *mockListener) Addr() net.Addr { return m.ln.Addr() }

func mustTempDir() string {
	path, err := ioutil.TempDir("", "caesium-test-")
	if err != nil {
		panic("failed to create temp dir")
	}
	return path
}

func mustParseDuration(t string) time.Duration {
	d, err := time.ParseDuration(t)
	if err != nil {
		panic("failed to parse duration")
	}
	return d
}

func executeRequestFromString(s string, timings, tx bool) *command.ExecuteRequest {
	return executeRequestFromStrings([]string{s}, timings, tx)
}

// queryRequestFromStrings converts a slice of strings into a command.QueryRequest
func executeRequestFromStrings(s []string, timings, tx bool) *command.ExecuteRequest {
	stmts := make([]*command.Statement, len(s))
	for i := range s {
		stmts[i] = &command.Statement{
			Sql: s[i],
		}

	}
	return &command.ExecuteRequest{
		Request: &command.Request{
			Statements:  stmts,
			Transaction: tx,
		},
		Timings: timings,
	}
}

func queryRequestFromString(s string, timings, tx bool) *command.QueryRequest {
	return queryRequestFromStrings([]string{s}, timings, tx)
}

// queryRequestFromStrings converts a slice of strings into a command.QueryRequest
func queryRequestFromStrings(s []string, timings, tx bool) *command.QueryRequest {
	stmts := make([]*command.Statement, len(s))
	for i := range s {
		stmts[i] = &command.Statement{
			Sql: s[i],
		}

	}
	return &command.QueryRequest{
		Request: &command.Request{
			Statements:  stmts,
			Transaction: tx,
		},
		Timings: timings,
	}
}

func asJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("failed to JSON marshal value")
	}
	return string(b)
}

func testPoll(t *testing.T, f func() bool, p time.Duration, d time.Duration) {
	tck := time.NewTicker(p)
	defer tck.Stop()
	tmr := time.NewTimer(d)
	defer tmr.Stop()

	for {
		select {
		case <-tck.C:
			if f() {
				return
			}
		case <-tmr.C:
			t.Fatalf("timeout expired: %s", t.Name())
		}
	}
}

func TestStoreTestSuite(t *testing.T) {
	suite.Run(t, new(StoreTestSuite))
}
