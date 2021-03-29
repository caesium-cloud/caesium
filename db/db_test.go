package db

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/caesium-cloud/caesium/db/command"
	"github.com/caesium-cloud/caesium/db/testdata/chinook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
)

type DBTestSuite struct {
	suite.Suite
	m sync.Map
}

func (s *DBTestSuite) SetupTest() {
	dir, err := ioutil.TempDir("", "caesium-test-")
	assert.Nil(s.T(), err)

	db, err := Open(path.Join(dir, "test_db"))
	assert.Nil(s.T(), err)
	s.m.Store(s.T().Name(), db)
}

func (s *DBTestSuite) TeardownTest() {
	assert.Nil(s.T(), s.DB().Close())
	assert.Nil(s.T(), os.RemoveAll(s.DB().path))
}

func (s *DBTestSuite) DB() *DB {
	db, ok := s.m.Load(s.T().Name())
	assert.True(s.T(), ok)
	assert.NotNil(s.T(), db)
	return db.(*DB)
}

func (s *DBTestSuite) TestTableCreation() {
	_, err := s.DB().ExecuteStringStmt("CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)")
	assert.Nil(s.T(), err)

	r, err := s.DB().QueryStringStmt("SELECT * FROM foo")
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id","name"],"types":["integer","text"]}]`, asJSON(r))
}

func (s *DBTestSuite) TestMasterTable() {
	_, err := s.DB().ExecuteStringStmt("CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)")
	assert.Nil(s.T(), err)

	r, err := s.DB().QueryStringStmt("SELECT * FROM sqlite_master")
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["type","name","tbl_name","rootpage","sql"],"types":["text","text","text","int","text"],"values":[["table","foo","foo",2,"CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)"]]}]`, asJSON(r))
}

func (s *DBTestSuite) TestLoadInMemory() {
	_, err := s.DB().ExecuteStringStmt("CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)")
	assert.Nil(s.T(), err)

	r, err := s.DB().QueryStringStmt("SELECT * FROM foo")
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id","name"],"types":["integer","text"]}]`, asJSON(r))

	inmem, err := LoadInMemoryWithDSN(s.DB().path, "")
	assert.Nil(s.T(), err)

	// Ensure it has been loaded correctly into the database
	r, err = inmem.QueryStringStmt("SELECT * FROM foo")
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id","name"],"types":["integer","text"]}]`, asJSON(r))
}

// ---------------------------------------------------------

func (s *DBTestSuite) TestDeserializeInMemoryWithDSN() {
	_, err := s.DB().ExecuteStringStmt("CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)")
	assert.Nil(s.T(), err)

	req := &command.Request{
		Transaction: true,
		Statements: []*command.Statement{
			{
				Sql: `INSERT INTO foo(id, name) VALUES(1, "bar")`,
			},
			{
				Sql: `INSERT INTO foo(id, name) VALUES(2, "bar")`,
			},
			{
				Sql: `INSERT INTO foo(id, name) VALUES(3, "bar")`,
			},
			{
				Sql: `INSERT INTO foo(id, name) VALUES(4, "bar")`,
			},
		},
	}

	_, err = s.DB().Execute(req, false)
	assert.Nil(s.T(), err)

	// Get byte representation of database on disk which, according to SQLite docs
	// is the same as a serialized version.
	b, err := ioutil.ReadFile(s.DB().path)
	assert.Nil(s.T(), err)

	newDB, err := DeserializeInMemoryWithDSN(b, "")
	assert.Nil(s.T(), err)
	defer newDB.Close()

	ro, err := newDB.QueryStringStmt(`SELECT * FROM foo`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id","name"],"types":["integer","text"],"values":[[1,"bar"],[2,"bar"],[3,"bar"],[4,"bar"]]}]`, asJSON(ro))

	// Write a lot of records to the new database, to ensure it's fully functional.
	req = &command.Request{
		Statements: []*command.Statement{
			{
				Sql: `INSERT INTO foo(name) VALUES("bar")`,
			},
		},
	}
	for i := 0; i < 5000; i++ {
		_, err = newDB.Execute(req, false)
		assert.Nil(s.T(), err)
	}
	ro, err = newDB.QueryStringStmt(`SELECT COUNT(*) FROM foo`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["COUNT(*)"],"types":[""],"values":[[5004]]}]`, asJSON(ro))
}

func (s *DBTestSuite) TestEmptyStatements() {
	_, err := s.DB().ExecuteStringStmt("")
	assert.Nil(s.T(), err)

	_, err = s.DB().ExecuteStringStmt(";")
	assert.Nil(s.T(), err)
}

func (s *DBTestSuite) TestSimpleSingleStatements() {
	_, err := s.DB().ExecuteStringStmt("CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)")
	assert.Nil(s.T(), err)

	_, err = s.DB().ExecuteStringStmt(`INSERT INTO foo(name) VALUES("bar")`)
	assert.Nil(s.T(), err)

	_, err = s.DB().ExecuteStringStmt(`INSERT INTO foo(name) VALUES("baz")`)
	assert.Nil(s.T(), err)

	r, err := s.DB().QueryStringStmt(`SELECT * FROM foo`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id","name"],"types":["integer","text"],"values":[[1,"bar"],[2,"baz"]]}]`, asJSON(r))

	r, err = s.DB().QueryStringStmt(`SELECT * FROM foo WHERE name="baz"`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id","name"],"types":["integer","text"],"values":[[2,"baz"]]}]`, asJSON(r))

	r, err = s.DB().QueryStringStmt(`SELECT * FROM foo WHERE name="qux"`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id","name"],"types":["integer","text"]}]`, asJSON(r))

	r, err = s.DB().QueryStringStmt(`SELECT * FROM foo ORDER BY name DESC`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id","name"],"types":["integer","text"],"values":[[2,"baz"],[1,"bar"]]}]`, asJSON(r))

	r, err = s.DB().QueryStringStmt(`SELECT *,name FROM foo`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id","name","name"],"types":["integer","text","text"],"values":[[1,"bar","bar"],[2,"baz","baz"]]}]`, asJSON(r))
}

func (s *DBTestSuite) TestSimpleSingleJSONStatements() {
	_, err := s.DB().ExecuteStringStmt("CREATE TABLE foo (c0 VARCHAR(36), c1 JSON, c2 NCHAR, c3 NVARCHAR, c4 CLOB)")
	assert.Nil(s.T(), err)

	_, err = s.DB().ExecuteStringStmt(`INSERT INTO foo(c0, c1, c2, c3, c4) VALUES("bar", '{"foo": "bar"}', "baz", "qux", "quux")`)
	assert.Nil(s.T(), err)

	r, err := s.DB().QueryStringStmt("SELECT * FROM foo")
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["c0","c1","c2","c3","c4"],"types":["varchar(36)","json","nchar","nvarchar","clob"],"values":[["bar","{\"foo\": \"bar\"}","baz","qux","quux"]]}]`, asJSON(r))
}

func (s *DBTestSuite) TestSimpleJoinStatements() {
	_, err := s.DB().ExecuteStringStmt("CREATE TABLE names (id INTEGER NOT NULL PRIMARY KEY, name TEXT, ssn TEXT)")
	assert.Nil(s.T(), err)

	req := &command.Request{
		Statements: []*command.Statement{
			{
				Sql: `INSERT INTO "names" VALUES(1,'bar','123-45-678')`,
			},
			{
				Sql: `INSERT INTO "names" VALUES(2,'baz','111-22-333')`,
			},
			{
				Sql: `INSERT INTO "names" VALUES(3,'qux','222-22-333')`,
			},
		},
	}
	_, err = s.DB().Execute(req, false)
	assert.Nil(s.T(), err)

	_, err = s.DB().ExecuteStringStmt("CREATE TABLE staff (id INTEGER NOT NULL PRIMARY KEY, employer TEXT, ssn TEXT)")
	assert.Nil(s.T(), err)

	_, err = s.DB().ExecuteStringStmt(`INSERT INTO "staff" VALUES(1,'quux','222-22-333')`)
	assert.Nil(s.T(), err)

	r, err := s.DB().QueryStringStmt(`SELECT names.id,name,names.ssn,employer FROM names INNER JOIN staff ON staff.ssn = names.ssn`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id","name","ssn","employer"],"types":["integer","text","text","text"],"values":[[3,"qux","222-22-333","quux"]]}]`, asJSON(r))
}

func (s *DBTestSuite) TestSimpleSingleConcatStatements() {
	_, err := s.DB().ExecuteStringStmt("CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)")
	assert.Nil(s.T(), err)

	_, err = s.DB().ExecuteStringStmt(`INSERT INTO foo(name) VALUES("bar")`)
	assert.Nil(s.T(), err)

	r, err := s.DB().QueryStringStmt(`SELECT id || "_bar", name FROM foo`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id || \"_bar\"","name"],"types":["","text"],"values":[["1_bar","bar"]]}]`, asJSON(r))
}

func (s *DBTestSuite) TestSimpleMultiStatements() {
	_, err := s.DB().ExecuteStringStmt("CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)")
	assert.Nil(s.T(), err)

	req := &command.Request{
		Statements: []*command.Statement{
			{
				Sql: `INSERT INTO foo(name) VALUES("bar")`,
			},
			{
				Sql: `INSERT INTO foo(name) VALUES("baz")`,
			},
		},
	}
	re, err := s.DB().Execute(req, false)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"last_insert_id":1,"rows_affected":1},{"last_insert_id":2,"rows_affected":1}]`, asJSON(re))

	req = &command.Request{
		Statements: []*command.Statement{
			{
				Sql: `SELECT * FROM foo`,
			},
			{
				Sql: `SELECT * FROM foo`,
			},
		},
	}
	ro, err := s.DB().Query(req, false)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id","name"],"types":["integer","text"],"values":[[1,"bar"],[2,"baz"]]},{"columns":["id","name"],"types":["integer","text"],"values":[[1,"bar"],[2,"baz"]]}]`, asJSON(ro))
}

func (s *DBTestSuite) TestSimpleSingleMultiLineStatements() {
	req := &command.Request{
		Statements: []*command.Statement{
			{
				Sql: `
CREATE TABLE foo (
id INTEGER NOT NULL PRIMARY KEY,
name TEXT
)`,
			},
		},
	}
	_, err := s.DB().Execute(req, false)
	assert.Nil(s.T(), err)

	req = &command.Request{
		Statements: []*command.Statement{
			{
				Sql: `INSERT INTO foo(name) VALUES("bar")`,
			},
			{
				Sql: `INSERT INTO foo(name) VALUES("baz")`,
			},
		},
	}
	re, err := s.DB().Execute(req, false)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"last_insert_id":1,"rows_affected":1},{"last_insert_id":2,"rows_affected":1}]`, asJSON(re))
}

func (s *DBTestSuite) TestSimpleFailingExecuteStatements() {
	r, err := s.DB().ExecuteStringStmt(`INSERT INTO foo(name) VALUES("bar")`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"error":"no such table: foo"}]`, asJSON(r))

	r, err = s.DB().ExecuteStringStmt(`CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{}]`, asJSON(r))

	r, err = s.DB().ExecuteStringStmt(`CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"error":"table foo already exists"}]`, asJSON(r))

	r, err = s.DB().ExecuteStringStmt(`INSERT INTO foo(id, name) VALUES(11, "bar")`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"last_insert_id":11,"rows_affected":1}]`, asJSON(r))

	r, err = s.DB().ExecuteStringStmt(`INSERT INTO foo(id, name) VALUES(11, "bar")`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"error":"UNIQUE constraint failed: foo.id"}]`, asJSON(r))

	r, err = s.DB().ExecuteStringStmt(`utter nonsense`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"error":"near \"utter\": syntax error"}]`, asJSON(r))
}

func (s *DBTestSuite) TestSimpleFailingQueryStatements() {
	ro, err := s.DB().QueryStringStmt(`SELECT * FROM bar`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"error":"no such table: bar"}]`, asJSON(ro))

	ro, err = s.DB().QueryStringStmt(`SELECTxx * FROM foo`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"error":"near \"SELECTxx\": syntax error"}]`, asJSON(ro))

	r, err := s.DB().QueryStringStmt(`utter nonsense`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"error":"near \"utter\": syntax error"}]`, asJSON(r))
}

func (s *DBTestSuite) TestSimplePragmaTableInfo() {
	r, err := s.DB().ExecuteStringStmt(`CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{}]`, asJSON(r))

	res, err := s.DB().QueryStringStmt(`PRAGMA table_info("foo")`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["cid","name","type","notnull","dflt_value","pk"],"types":["","","","","",""],"values":[[0,"id","INTEGER",1,null,1],[1,"name","TEXT",0,null,0]]}]`, asJSON(res))
}

func (s *DBTestSuite) TestSimpleParameterizedStatements() {
	_, err := s.DB().ExecuteStringStmt("CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)")
	assert.Nil(s.T(), err)

	req := &command.Request{
		Statements: []*command.Statement{
			{
				Sql: "INSERT INTO foo(name) VALUES(?)",
				Parameters: []*command.Parameter{
					{
						Value: &command.Parameter_S{
							S: "bar",
						},
					},
				},
			},
		},
	}
	_, err = s.DB().Execute(req, false)
	assert.Nil(s.T(), err)

	req.Statements[0].Parameters[0] = &command.Parameter{
		Value: &command.Parameter_S{
			S: "baz",
		},
	}
	_, err = s.DB().Execute(req, false)
	assert.Nil(s.T(), err)

	r, err := s.DB().QueryStringStmt(`SELECT * FROM foo`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id","name"],"types":["integer","text"],"values":[[1,"bar"],[2,"baz"]]}]`, asJSON(r))

	req.Statements[0].Sql = "SELECT * FROM foo WHERE name=?"
	req.Statements[0].Parameters[0] = &command.Parameter{
		Value: &command.Parameter_S{
			S: "baz",
		},
	}
	r, err = s.DB().Query(req, false)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id","name"],"types":["integer","text"],"values":[[2,"baz"]]}]`, asJSON(r))

	req.Statements[0].Parameters[0] = &command.Parameter{
		Value: &command.Parameter_S{
			S: "bar",
		},
	}
	r, err = s.DB().Query(req, false)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id","name"],"types":["integer","text"],"values":[[1,"bar"]]}]`, asJSON(r))

	req = &command.Request{
		Statements: []*command.Statement{
			{
				Sql: "SELECT * FROM foo WHERE NAME=?",
				Parameters: []*command.Parameter{
					{
						Value: &command.Parameter_S{
							S: "bar",
						},
					},
				},
			},
			{
				Sql: "SELECT * FROM foo WHERE NAME=?",
				Parameters: []*command.Parameter{
					{
						Value: &command.Parameter_S{
							S: "baz",
						},
					},
				},
			},
		},
	}
	r, err = s.DB().Query(req, false)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id","name"],"types":["integer","text"],"values":[[1,"bar"]]},{"columns":["id","name"],"types":["integer","text"],"values":[[2,"baz"]]}]`, asJSON(r))
}

func (s *DBTestSuite) TestCommonTableExpressions() {
	_, err := s.DB().ExecuteStringStmt("CREATE TABLE test(x foo)")
	assert.Nil(s.T(), err)

	_, err = s.DB().ExecuteStringStmt(`INSERT INTO test VALUES(1)`)
	assert.Nil(s.T(), err)

	r, err := s.DB().QueryStringStmt(`WITH bar AS (SELECT * FROM test) SELECT * FROM test WHERE x = 1`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["x"],"types":["foo"],"values":[[1]]}]`, asJSON(r))

	r, err = s.DB().QueryStringStmt(`WITH bar AS (SELECT * FROM test) SELECT * FROM test WHERE x = 2`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["x"],"types":["foo"]}]`, asJSON(r))
}

func (s *DBTestSuite) TestForeignKeyConstraints() {
	_, err := s.DB().ExecuteStringStmt("CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, ref INTEGER REFERENCES foo(id))")
	assert.Nil(s.T(), err)

	// Explicitly disable constraints.
	assert.Nil(s.T(), s.DB().EnableFKConstraints(false))

	// Check constraints
	fk, err := s.DB().FKConstraints()
	assert.Nil(s.T(), err)
	assert.False(s.T(), fk)

	r, err := s.DB().ExecuteStringStmt(`INSERT INTO foo(id, ref) VALUES(1, 2)`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"last_insert_id":1,"rows_affected":1}]`, asJSON(r))

	// Explicitly enable constraints.
	assert.Nil(s.T(), s.DB().EnableFKConstraints(true))

	// Check constraints
	fk, err = s.DB().FKConstraints()
	assert.Nil(s.T(), err)
	assert.True(s.T(), fk)

	r, err = s.DB().ExecuteStringStmt(`INSERT INTO foo(id, ref) VALUES(1, 3)`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"error":"UNIQUE constraint failed: foo.id"}]`, asJSON(r))
}

func (s *DBTestSuite) TestUniqueConstraints() {
	_, err := s.DB().ExecuteStringStmt("CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT, CONSTRAINT name_unique UNIQUE (name))")
	assert.Nil(s.T(), err)

	r, err := s.DB().ExecuteStringStmt(`INSERT INTO foo(name) VALUES("bar")`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"last_insert_id":1,"rows_affected":1}]`, asJSON(r))

	// UNIQUE constraint should fire.
	r, err = s.DB().ExecuteStringStmt(`INSERT INTO foo(name) VALUES("bar")`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"error":"UNIQUE constraint failed: foo.name"}]`, asJSON(r))
}

func (s *DBTestSuite) TestDBSize() {
	_, err := s.DB().Size()
	assert.Nil(s.T(), err)

	_, err = s.DB().FileSize()
	assert.Nil(s.T(), err)
}

func (s *DBTestSuite) TestActiveTransaction() {
	assert.False(s.T(), s.DB().TransactionActive())

	_, err := s.DB().ExecuteStringStmt(`BEGIN`)
	assert.Nil(s.T(), err)
	assert.True(s.T(), s.DB().TransactionActive())

	_, err = s.DB().ExecuteStringStmt(`COMMIT`)
	assert.Nil(s.T(), err)
	assert.False(s.T(), s.DB().TransactionActive())

	_, err = s.DB().ExecuteStringStmt(`BEGIN`)
	assert.Nil(s.T(), err)
	assert.True(s.T(), s.DB().TransactionActive())

	_, err = s.DB().ExecuteStringStmt(`ROLLBACK`)
	assert.Nil(s.T(), err)
	assert.False(s.T(), s.DB().TransactionActive())
}

func (s *DBTestSuite) TestAbortTransaction() {
	assert.Nil(s.T(), s.DB().AbortTransaction())

	_, err := s.DB().ExecuteStringStmt(`BEGIN`)
	assert.Nil(s.T(), err)
	assert.True(s.T(), s.DB().TransactionActive())

	assert.Nil(s.T(), s.DB().AbortTransaction())
	assert.False(s.T(), s.DB().TransactionActive())
}

func (s *DBTestSuite) TestPartialFailure() {
	_, err := s.DB().ExecuteStringStmt("CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)")
	assert.Nil(s.T(), err)

	req := &command.Request{
		Statements: []*command.Statement{
			{
				Sql: `INSERT INTO foo(id, name) VALUES(1, "bar")`,
			},
			{
				Sql: `INSERT INTO foo(id, name) VALUES(2, "bar")`,
			},
			{
				Sql: `INSERT INTO foo(id, name) VALUES(1, "bar")`,
			},
			{
				Sql: `INSERT INTO foo(id, name) VALUES(4, "bar")`,
			},
		},
	}
	r, err := s.DB().Execute(req, false)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"last_insert_id":1,"rows_affected":1},{"last_insert_id":2,"rows_affected":1},{"error":"UNIQUE constraint failed: foo.id"},{"last_insert_id":4,"rows_affected":1}]`, asJSON(r))

	ro, err := s.DB().QueryStringStmt(`SELECT * FROM foo`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id","name"],"types":["integer","text"],"values":[[1,"bar"],[2,"bar"],[4,"bar"]]}]`, asJSON(ro))
}

func (s *DBTestSuite) TestSimpleTransaction() {
	_, err := s.DB().ExecuteStringStmt("CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)")
	assert.Nil(s.T(), err)

	req := &command.Request{
		Transaction: true,
		Statements: []*command.Statement{
			{
				Sql: `INSERT INTO foo(id, name) VALUES(1, "bar")`,
			},
			{
				Sql: `INSERT INTO foo(id, name) VALUES(2, "bar")`,
			},
			{
				Sql: `INSERT INTO foo(id, name) VALUES(3, "bar")`,
			},
			{
				Sql: `INSERT INTO foo(id, name) VALUES(4, "bar")`,
			},
		},
	}
	r, err := s.DB().Execute(req, false)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"last_insert_id":1,"rows_affected":1},{"last_insert_id":2,"rows_affected":1},{"last_insert_id":3,"rows_affected":1},{"last_insert_id":4,"rows_affected":1}]`, asJSON(r))

	ro, err := s.DB().QueryStringStmt(`SELECT * FROM foo`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id","name"],"types":["integer","text"],"values":[[1,"bar"],[2,"bar"],[3,"bar"],[4,"bar"]]}]`, asJSON(ro))
}

func (s *DBTestSuite) TestPartialFailureTransaction() {
	_, err := s.DB().ExecuteStringStmt("CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)")
	assert.Nil(s.T(), err)

	req := &command.Request{
		Transaction: true,
		Statements: []*command.Statement{
			{
				Sql: `INSERT INTO foo(id, name) VALUES(1, "bar")`,
			},
			{
				Sql: `INSERT INTO foo(id, name) VALUES(2, "bar")`,
			},
			{
				Sql: `INSERT INTO foo(id, name) VALUES(1, "bar")`,
			},
			{
				Sql: `INSERT INTO foo(id, name) VALUES(4, "bar")`,
			},
		},
	}
	r, err := s.DB().Execute(req, false)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"last_insert_id":1,"rows_affected":1},{"last_insert_id":2,"rows_affected":1},{"error":"UNIQUE constraint failed: foo.id"}]`, asJSON(r))

	ro, err := s.DB().QueryStringStmt(`SELECT * FROM foo`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id","name"],"types":["integer","text"]}]`, asJSON(ro))
}

func (s *DBTestSuite) TestBackup() {
	_, err := s.DB().ExecuteStringStmt("CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)")
	assert.Nil(s.T(), err)

	req := &command.Request{
		Transaction: true,
		Statements: []*command.Statement{
			{
				Sql: `INSERT INTO foo(id, name) VALUES(1, "bar")`,
			},
			{
				Sql: `INSERT INTO foo(id, name) VALUES(2, "bar")`,
			},
			{
				Sql: `INSERT INTO foo(id, name) VALUES(3, "bar")`,
			},
			{
				Sql: `INSERT INTO foo(id, name) VALUES(4, "bar")`,
			},
		},
	}
	_, err = s.DB().Execute(req, false)
	assert.Nil(s.T(), err)

	dstDB := mustTempFile()
	defer os.Remove(dstDB)

	assert.Nil(s.T(), s.DB().Backup(dstDB))

	newDB, err := Open(dstDB)
	assert.Nil(s.T(), err)
	defer newDB.Close()
	defer os.Remove(dstDB)
	ro, err := newDB.QueryStringStmt(`SELECT * FROM foo`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id","name"],"types":["integer","text"],"values":[[1,"bar"],[2,"bar"],[3,"bar"],[4,"bar"]]}]`, asJSON(ro))
}

func (s *DBTestSuite) TestCopy() {
	srcDB := s.DB()

	_, err := srcDB.ExecuteStringStmt("CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)")
	assert.Nil(s.T(), err)

	req := &command.Request{
		Transaction: true,
		Statements: []*command.Statement{
			{
				Sql: `INSERT INTO foo(id, name) VALUES(1, "bar")`,
			},
			{
				Sql: `INSERT INTO foo(id, name) VALUES(2, "bar")`,
			},
			{
				Sql: `INSERT INTO foo(id, name) VALUES(3, "bar")`,
			},
			{
				Sql: `INSERT INTO foo(id, name) VALUES(4, "bar")`,
			},
		},
	}
	_, err = srcDB.Execute(req, false)
	assert.Nil(s.T(), err)

	dstFile := mustTempFile()
	defer os.Remove(dstFile)
	dstDB, err := Open(dstFile)
	assert.Nil(s.T(), err)
	defer dstDB.Close()

	assert.Nil(s.T(), srcDB.Copy(dstDB))

	ro, err := dstDB.QueryStringStmt(`SELECT * FROM foo`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id","name"],"types":["integer","text"],"values":[[1,"bar"],[2,"bar"],[3,"bar"],[4,"bar"]]}]`, asJSON(ro))
}

func (s *DBTestSuite) TestSerialize() {
	_, err := s.DB().ExecuteStringStmt("CREATE TABLE foo (id INTEGER NOT NULL PRIMARY KEY, name TEXT)")
	assert.Nil(s.T(), err)

	req := &command.Request{
		Transaction: true,
		Statements: []*command.Statement{
			{
				Sql: `INSERT INTO foo(id, name) VALUES(1, "bar")`,
			},
			{
				Sql: `INSERT INTO foo(id, name) VALUES(2, "bar")`,
			},
			{
				Sql: `INSERT INTO foo(id, name) VALUES(3, "bar")`,
			},
			{
				Sql: `INSERT INTO foo(id, name) VALUES(4, "bar")`,
			},
		},
	}
	_, err = s.DB().Execute(req, false)
	assert.Nil(s.T(), err)

	dstDB, err := ioutil.TempFile("", "caesium-bak-")
	assert.Nil(s.T(), err)
	dstDB.Close()
	defer os.Remove(dstDB.Name())

	// Get the bytes, and write to a temp file.
	b, err := s.DB().Serialize()
	assert.Nil(s.T(), err)
	assert.Nil(s.T(), ioutil.WriteFile(dstDB.Name(), b, 0644))

	newDB, err := Open(dstDB.Name())
	assert.Nil(s.T(), err)
	defer newDB.Close()
	defer os.Remove(dstDB.Name())
	ro, err := newDB.QueryStringStmt(`SELECT * FROM foo`)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), `[{"columns":["id","name"],"types":["integer","text"],"values":[[1,"bar"],[2,"bar"],[3,"bar"],[4,"bar"]]}]`, asJSON(ro))
}

func (s *DBTestSuite) TestDump() {
	_, err := s.DB().ExecuteStringStmt(chinook.DB)
	assert.Nil(s.T(), err)

	var b strings.Builder
	assert.Nil(s.T(), s.DB().Dump(&b))
	assert.Equal(s.T(), chinook.DB, b.String())
}

func (s *DBTestSuite) TestDumpMemory() {
	inmem, err := LoadInMemoryWithDSN(s.DB().path, "")
	assert.Nil(s.T(), err)

	_, err = inmem.ExecuteStringStmt(chinook.DB)
	assert.Nil(s.T(), err)

	var b strings.Builder
	assert.Nil(s.T(), inmem.Dump(&b))
	assert.Equal(s.T(), chinook.DB, b.String())
}

func asJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("failed to JSON marshal value")
	}
	return string(b)
}

// mustTempFile returns a path to a temporary file in directory dir. It is up to the
// caller to remove the file once it is no longer needed.
func mustTempFile() string {
	tmpfile, err := ioutil.TempFile("", "caesium-db-test")
	if err != nil {
		panic(err.Error())
	}
	tmpfile.Close()
	return tmpfile.Name()
}

func TestDBTestSuite(t *testing.T) {
	suite.Run(t, new(DBTestSuite))
}
