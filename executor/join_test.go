// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package executor_test

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/executor"
	plannercore "github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/session"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/testkit"
	"github.com/pingcap/tidb/testkit/testdata"
	"github.com/pingcap/tidb/util"
	"github.com/stretchr/testify/require"
)

func TestJoinPanic2(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("set sql_mode = 'ONLY_FULL_GROUP_BY'")
	tk.MustExec("drop table if exists events")
	tk.MustExec("create table events (clock int, source int)")
	tk.MustQuery("SELECT * FROM events e JOIN (SELECT MAX(clock) AS clock FROM events e2 GROUP BY e2.source) e3 ON e3.clock=e.clock")
	err := tk.ExecToErr("SELECT * FROM events e JOIN (SELECT clock FROM events e2 GROUP BY e2.source) e3 ON e3.clock=e.clock")
	require.Error(t, err)

	// Test for PR 18983, use to detect race.
	tk.MustExec("use test")
	tk.MustExec("drop table if exists tpj1,tpj2;")
	tk.MustExec("create table tpj1 (id int, b int,  unique index (id));")
	tk.MustExec("create table tpj2 (id int, b int,  unique index (id));")
	tk.MustExec("insert into tpj1 values  (1,1);")
	tk.MustExec("insert into tpj2 values  (1,1);")
	tk.MustQuery("select tpj1.b,tpj2.b from tpj1 left join tpj2 on tpj1.id=tpj2.id where tpj1.id=1;").Check(testkit.Rows("1 1"))
}

func TestJoinInDisk(t *testing.T) {
	origin := config.RestoreFunc()
	defer origin()

	store, dom := testkit.CreateMockStoreAndDomain(t)
	tk := testkit.NewTestKit(t, store)
	defer tk.MustExec("SET GLOBAL tidb_mem_oom_action = DEFAULT")
	tk.MustExec("SET GLOBAL tidb_mem_oom_action='LOG'")
	tk.MustExec("use test")

	sm := &testkit.MockSessionManager{
		PS: make([]*util.ProcessInfo, 0),
	}
	tk.Session().SetSessionManager(sm)
	dom.ExpensiveQueryHandle().SetSessionManager(sm)

	// TODO(fengliyuan): how to ensure that it is using disk really?
	tk.MustExec("set @@tidb_mem_quota_query=1;")
	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t(c1 int, c2 int)")
	tk.MustExec("create table t1(c1 int, c2 int)")
	tk.MustExec("insert into t values(1,1),(2,2)")
	tk.MustExec("insert into t1 values(2,3),(4,4)")
	result := tk.MustQuery("select /*+ TIDB_HJ(t, t2) */ * from t, t1 where t.c1 = t1.c1")
	result.Check(testkit.Rows("2 2 2 3"))
}

func TestJoin2(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)

	tk.MustExec("set @@tidb_index_lookup_join_concurrency = 200")
	require.Equal(t, 200, tk.Session().GetSessionVars().IndexLookupJoinConcurrency())

	tk.MustExec("set @@tidb_index_lookup_join_concurrency = 4")
	require.Equal(t, 4, tk.Session().GetSessionVars().IndexLookupJoinConcurrency())

	tk.MustExec("set @@tidb_index_lookup_size = 2")
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t (c int)")
	tk.MustExec("insert t values (1)")
	tests := []struct {
		sql    string
		result [][]interface{}
	}{
		{
			"select 1 from t as a left join t as b on 0",
			testkit.Rows("1"),
		},
		{
			"select 1 from t as a join t as b on 1",
			testkit.Rows("1"),
		},
	}
	for _, tt := range tests {
		result := tk.MustQuery(tt.sql)
		result.Check(tt.result)
	}

	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t(c1 int, c2 int)")
	tk.MustExec("create table t1(c1 int, c2 int)")
	tk.MustExec("insert into t values(1,1),(2,2)")
	tk.MustExec("insert into t1 values(2,3),(4,4)")
	result := tk.MustQuery("select * from t left outer join t1 on t.c1 = t1.c1 where t.c1 = 1 or t1.c2 > 20")
	result.Check(testkit.Rows("1 1 <nil> <nil>"))
	result = tk.MustQuery("select * from t1 right outer join t on t.c1 = t1.c1 where t.c1 = 1 or t1.c2 > 20")
	result.Check(testkit.Rows("<nil> <nil> 1 1"))
	result = tk.MustQuery("select * from t right outer join t1 on t.c1 = t1.c1 where t.c1 = 1 or t1.c2 > 20")
	result.Check(testkit.Rows())
	result = tk.MustQuery("select * from t left outer join t1 on t.c1 = t1.c1 where t1.c1 = 3 or false")
	result.Check(testkit.Rows())
	result = tk.MustQuery("select * from t left outer join t1 on t.c1 = t1.c1 and t.c1 != 1 order by t1.c1")
	result.Check(testkit.Rows("1 1 <nil> <nil>", "2 2 2 3"))
	result = tk.MustQuery("select t.c1, t1.c1 from t left outer join t1 on t.c1 = t1.c1 and t.c2 + t1.c2 <= 5")
	result.Check(testkit.Rows("1 <nil>", "2 2"))

	tk.MustExec("drop table if exists t1")
	tk.MustExec("drop table if exists t2")
	tk.MustExec("drop table if exists t3")

	tk.MustExec("create table t1 (c1 int, c2 int)")
	tk.MustExec("create table t2 (c1 int, c2 int)")
	tk.MustExec("create table t3 (c1 int, c2 int)")

	tk.MustExec("insert into t1 values (1,1), (2,2), (3,3)")
	tk.MustExec("insert into t2 values (1,1), (3,3), (5,5)")
	tk.MustExec("insert into t3 values (1,1), (5,5), (9,9)")

	result = tk.MustQuery("select * from t1 left join t2 on t1.c1 = t2.c1 right join t3 on t2.c1 = t3.c1 order by t1.c1, t1.c2, t2.c1, t2.c2, t3.c1, t3.c2;")
	result.Check(testkit.Rows("<nil> <nil> <nil> <nil> 5 5", "<nil> <nil> <nil> <nil> 9 9", "1 1 1 1 1 1"))

	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t1 (c1 int)")
	tk.MustExec("insert into t1 values (1), (1), (1)")
	result = tk.MustQuery("select * from t1 a join t1 b on a.c1 = b.c1;")
	result.Check(testkit.Rows("1 1", "1 1", "1 1", "1 1", "1 1", "1 1", "1 1", "1 1", "1 1"))

	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t(c1 int, index k(c1))")
	tk.MustExec("create table t1(c1 int)")
	tk.MustExec("insert into t values (1),(2),(3),(4),(5),(6),(7)")
	tk.MustExec("insert into t1 values (1),(2),(3),(4),(5),(6),(7)")
	result = tk.MustQuery("select a.c1 from t a , t1 b where a.c1 = b.c1 order by a.c1;")
	result.Check(testkit.Rows("1", "2", "3", "4", "5", "6", "7"))
	// Test race.
	result = tk.MustQuery("select a.c1 from t a , t1 b where a.c1 = b.c1 and a.c1 + b.c1 > 5 order by b.c1")
	result.Check(testkit.Rows("3", "4", "5", "6", "7"))
	result = tk.MustQuery("select a.c1 from t a , (select * from t1 limit 3) b where a.c1 = b.c1 order by b.c1;")
	result.Check(testkit.Rows("1", "2", "3"))

	tk.MustExec("drop table if exists t,t2,t1")
	tk.MustExec("create table t(c1 int)")
	tk.MustExec("create table t1(c1 int, c2 int)")
	tk.MustExec("create table t2(c1 int, c2 int)")
	tk.MustExec("insert into t1 values(1,2),(2,3),(3,4)")
	tk.MustExec("insert into t2 values(1,0),(2,0),(3,0)")
	tk.MustExec("insert into t values(1),(2),(3)")
	result = tk.MustQuery("select * from t1 , t2 where t2.c1 = t1.c1 and t2.c2 = 0 and t1.c2 in (select * from t)")
	result.Sort().Check(testkit.Rows("1 2 1 0", "2 3 2 0"))
	result = tk.MustQuery("select * from t1 , t2 where t2.c1 = t1.c1 and t2.c2 = 0 and t1.c1 = 1 order by t1.c2 limit 1")
	result.Sort().Check(testkit.Rows("1 2 1 0"))
	tk.MustExec("drop table if exists t, t1")
	tk.MustExec("create table t(a int primary key, b int)")
	tk.MustExec("create table t1(a int, b int, key s(b))")
	tk.MustExec("insert into t values(1, 1), (2, 2), (3, 3)")
	tk.MustExec("insert into t1 values(1, 2), (1, 3), (1, 4), (3, 4), (4, 5)")

	// The physical plans of the two sql are tested at physical_plan_test.go
	tk.MustQuery("select /*+ INL_JOIN(t, t1) */ * from t join t1 on t.a=t1.a").Check(testkit.Rows("1 1 1 2", "1 1 1 3", "1 1 1 4", "3 3 3 4"))
	tk.MustQuery("select /*+ INL_HASH_JOIN(t, t1) */ * from t join t1 on t.a=t1.a").Sort().Check(testkit.Rows("1 1 1 2", "1 1 1 3", "1 1 1 4", "3 3 3 4"))
	tk.MustQuery("select /*+ INL_MERGE_JOIN(t, t1) */ * from t join t1 on t.a=t1.a").Check(testkit.Rows("1 1 1 2", "1 1 1 3", "1 1 1 4", "3 3 3 4"))
	tk.MustQuery("select /*+ INL_JOIN(t) */ * from t1 join t on t.a=t1.a and t.a < t1.b").Check(testkit.Rows("1 2 1 1", "1 3 1 1", "1 4 1 1", "3 4 3 3"))
	tk.MustQuery("select /*+ INL_HASH_JOIN(t) */ * from t1 join t on t.a=t1.a and t.a < t1.b").Sort().Check(testkit.Rows("1 2 1 1", "1 3 1 1", "1 4 1 1", "3 4 3 3"))
	tk.MustQuery("select /*+ INL_MERGE_JOIN(t) */ * from t1 join t on t.a=t1.a and t.a < t1.b").Check(testkit.Rows("1 2 1 1", "1 3 1 1", "1 4 1 1", "3 4 3 3"))
	// Test single index reader.
	tk.MustQuery("select /*+ INL_JOIN(t, t1) */ t1.b from t1 join t on t.b=t1.b").Check(testkit.Rows("2", "3"))
	tk.MustQuery("select /*+ INL_HASH_JOIN(t, t1) */ t1.b from t1 join t on t.b=t1.b").Sort().Check(testkit.Rows("2", "3"))
	tk.MustQuery("select /*+ INL_MERGE_JOIN(t, t1) */ t1.b from t1 join t on t.b=t1.b").Check(testkit.Rows("2", "3"))
	tk.MustQuery("select /*+ INL_JOIN(t1) */ * from t right outer join t1 on t.a=t1.a").Sort().Check(testkit.Rows("1 1 1 2", "1 1 1 3", "1 1 1 4", "3 3 3 4", "<nil> <nil> 4 5"))
	tk.MustQuery("select /*+ INL_HASH_JOIN(t1) */ * from t right outer join t1 on t.a=t1.a").Sort().Check(testkit.Rows("1 1 1 2", "1 1 1 3", "1 1 1 4", "3 3 3 4", "<nil> <nil> 4 5"))
	tk.MustQuery("select /*+ INL_MERGE_JOIN(t1) */ * from t right outer join t1 on t.a=t1.a").Sort().Check(testkit.Rows("1 1 1 2", "1 1 1 3", "1 1 1 4", "3 3 3 4", "<nil> <nil> 4 5"))
	tk.MustQuery("select /*+ INL_JOIN(t) */ avg(t.b) from t right outer join t1 on t.a=t1.a").Check(testkit.Rows("1.5000"))
	tk.MustQuery("select /*+ INL_HASH_JOIN(t) */ avg(t.b) from t right outer join t1 on t.a=t1.a").Check(testkit.Rows("1.5000"))
	tk.MustQuery("select /*+ INL_MERGE_JOIN(t) */ avg(t.b) from t right outer join t1 on t.a=t1.a").Check(testkit.Rows("1.5000"))

	// Test that two conflict hints will return warning.
	tk.MustExec("select /*+ TIDB_INLJ(t) TIDB_SMJ(t) */ * from t join t1 on t.a=t1.a")
	require.Len(t, tk.Session().GetSessionVars().StmtCtx.GetWarnings(), 1)
	tk.MustExec("select /*+ TIDB_INLJ(t) TIDB_HJ(t) */ * from t join t1 on t.a=t1.a")
	require.Len(t, tk.Session().GetSessionVars().StmtCtx.GetWarnings(), 1)
	tk.MustExec("select /*+ TIDB_SMJ(t) TIDB_HJ(t) */ * from t join t1 on t.a=t1.a")
	require.Len(t, tk.Session().GetSessionVars().StmtCtx.GetWarnings(), 1)

	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int)")
	tk.MustExec("insert into t values(1),(2), (3)")
	tk.MustQuery("select @a := @a + 1 from t, (select @a := 0) b;").Check(testkit.Rows("1", "2", "3"))

	tk.MustExec("drop table if exists t, t1")
	tk.MustExec("create table t(a int primary key, b int, key s(b))")
	tk.MustExec("create table t1(a int, b int)")
	tk.MustExec("insert into t values(1, 3), (2, 2), (3, 1)")
	tk.MustExec("insert into t1 values(0, 0), (1, 2), (1, 3), (3, 4)")
	tk.MustQuery("select /*+ INL_JOIN(t1) */ * from t join t1 on t.a=t1.a order by t.b").Sort().Check(testkit.Rows("1 3 1 2", "1 3 1 3", "3 1 3 4"))
	tk.MustQuery("select /*+ INL_HASH_JOIN(t1) */ * from t join t1 on t.a=t1.a order by t.b").Sort().Check(testkit.Rows("1 3 1 2", "1 3 1 3", "3 1 3 4"))
	tk.MustQuery("select /*+ INL_MERGE_JOIN(t1) */ * from t join t1 on t.a=t1.a order by t.b").Sort().Check(testkit.Rows("1 3 1 2", "1 3 1 3", "3 1 3 4"))
	tk.MustQuery("select /*+ INL_JOIN(t) */ t.a, t.b from t join t1 on t.a=t1.a where t1.b = 4 limit 1").Check(testkit.Rows("3 1"))
	tk.MustQuery("select /*+ INL_HASH_JOIN(t) */ t.a, t.b from t join t1 on t.a=t1.a where t1.b = 4 limit 1").Check(testkit.Rows("3 1"))
	tk.MustQuery("select /*+ INL_MERGE_JOIN(t) */ t.a, t.b from t join t1 on t.a=t1.a where t1.b = 4 limit 1").Check(testkit.Rows("3 1"))
	tk.MustQuery("select /*+ INL_JOIN(t, t1) */ * from t right join t1 on t.a=t1.a order by t.b").Sort().Check(testkit.Rows("1 3 1 2", "1 3 1 3", "3 1 3 4", "<nil> <nil> 0 0"))
	tk.MustQuery("select /*+ INL_HASH_JOIN(t, t1) */ * from t right join t1 on t.a=t1.a order by t.b").Sort().Check(testkit.Rows("1 3 1 2", "1 3 1 3", "3 1 3 4", "<nil> <nil> 0 0"))
	tk.MustQuery("select /*+ INL_MERGE_JOIN(t, t1) */ * from t right join t1 on t.a=t1.a order by t.b").Sort().Check(testkit.Rows("1 3 1 2", "1 3 1 3", "3 1 3 4", "<nil> <nil> 0 0"))

	// join reorder will disorganize the resulting schema
	tk.MustExec("drop table if exists t, t1")
	tk.MustExec("create table t(a int, b int)")
	tk.MustExec("create table t1(a int, b int)")
	tk.MustExec("insert into t values(1,2)")
	tk.MustExec("insert into t1 values(3,4)")
	tk.MustQuery("select (select t1.a from t1 , t where t.a = s.a limit 2) from t as s").Check(testkit.Rows("3"))

	// test index join bug
	tk.MustExec("drop table if exists t, t1")
	tk.MustExec("create table t(a int, b int, key s1(a,b), key s2(b))")
	tk.MustExec("create table t1(a int)")
	tk.MustExec("insert into t values(1,2), (5,3), (6,4)")
	tk.MustExec("insert into t1 values(1), (2), (3)")
	tk.MustQuery("select /*+ INL_JOIN(t) */ t1.a from t1, t where t.a = 5 and t.b = t1.a").Check(testkit.Rows("3"))
	tk.MustQuery("select /*+ INL_HASH_JOIN(t) */ t1.a from t1, t where t.a = 5 and t.b = t1.a").Check(testkit.Rows("3"))
	tk.MustQuery("select /*+ INL_MERGE_JOIN(t) */ t1.a from t1, t where t.a = 5 and t.b = t1.a").Check(testkit.Rows("3"))

	// test issue#4997
	tk.MustExec("drop table if exists t1, t2")
	tk.MustExec(`
	CREATE TABLE t1 (
  		pk int(11) NOT NULL AUTO_INCREMENT primary key,
  		a int(11) DEFAULT NULL,
  		b date DEFAULT NULL,
  		c varchar(1) DEFAULT NULL,
  		KEY a (a),
  		KEY b (b),
  		KEY c (c,a)
	)`)
	tk.MustExec(`
	CREATE TABLE t2 (
  		pk int(11) NOT NULL AUTO_INCREMENT primary key,
  		a int(11) DEFAULT NULL,
  		b date DEFAULT NULL,
  		c varchar(1) DEFAULT NULL,
  		KEY a (a),
  		KEY b (b),
  		KEY c (c,a)
	)`)
	tk.MustExec(`insert into t1 value(1,1,"2000-11-11", null);`)
	result = tk.MustQuery(`
	SELECT table2.b AS field2 FROM
	(
	  t1 AS table1  LEFT OUTER JOIN
		(SELECT tmp_t2.* FROM ( t2 AS tmp_t1 RIGHT JOIN t1 AS tmp_t2 ON (tmp_t2.a = tmp_t1.a))) AS table2
	  ON (table2.c = table1.c)
	) `)
	result.Check(testkit.Rows("<nil>"))

	// test virtual rows are included (issue#5771)
	result = tk.MustQuery(`SELECT 1 FROM (SELECT 1) t1, (SELECT 1) t2`)
	result.Check(testkit.Rows("1"))

	result = tk.MustQuery(`
		SELECT @NUM := @NUM + 1 as NUM FROM
		( SELECT 1 UNION ALL
			SELECT 2 UNION ALL
			SELECT 3
		) a
		INNER JOIN
		( SELECT 1 UNION ALL
			SELECT 2 UNION ALL
			SELECT 3
		) b,
		(SELECT @NUM := 0) d;
	`)
	result.Check(testkit.Rows("1", "2", "3", "4", "5", "6", "7", "8", "9"))

	// This case is for testing:
	// when the main thread calls Executor.Close() while the out data fetch worker and join workers are still working,
	// we need to stop the goroutines as soon as possible to avoid unexpected error.
	tk.MustExec("set @@tidb_hash_join_concurrency=5")
	tk.MustExec("drop table if exists t;")
	tk.MustExec("create table t(a int)")
	for i := 0; i < 100; i++ {
		tk.MustExec("insert into t value(1)")
	}
	result = tk.MustQuery("select /*+ TIDB_HJ(s, r) */ * from t as s join t as r on s.a = r.a limit 1;")
	result.Check(testkit.Rows("1 1"))

	tk.MustExec("drop table if exists user, aa, bb")
	tk.MustExec("create table aa(id int)")
	tk.MustExec("insert into aa values(1)")
	tk.MustExec("create table bb(id int)")
	tk.MustExec("insert into bb values(1)")
	tk.MustExec("create table user(id int, name varchar(20))")
	tk.MustExec("insert into user values(1, 'a'), (2, 'b')")
	tk.MustQuery("select user.id,user.name from user left join aa on aa.id = user.id left join bb on aa.id = bb.id where bb.id < 10;").Check(testkit.Rows("1 a"))

	tk.MustExec(`drop table if exists t;`)
	tk.MustExec(`create table t (a bigint);`)
	tk.MustExec(`insert into t values (1);`)
	tk.MustQuery(`select t2.a, t1.a from t t1 inner join (select "1" as a) t2 on t2.a = t1.a;`).Check(testkit.Rows("1 1"))
	tk.MustQuery(`select t2.a, t1.a from t t1 inner join (select "2" as b, "1" as a) t2 on t2.a = t1.a;`).Check(testkit.Rows("1 1"))

	tk.MustExec("drop table if exists t1, t2, t3, t4")
	tk.MustExec("create table t1(a int, b int)")
	tk.MustExec("create table t2(a int, b int)")
	tk.MustExec("create table t3(a int, b int)")
	tk.MustExec("create table t4(a int, b int)")
	tk.MustExec("insert into t1 values(1, 1)")
	tk.MustExec("insert into t2 values(1, 1)")
	tk.MustExec("insert into t3 values(1, 1)")
	tk.MustExec("insert into t4 values(1, 1)")
	tk.MustQuery("select min(t2.b) from t1 right join t2 on t2.a=t1.a right join t3 on t2.a=t3.a left join t4 on t3.a=t4.a").Check(testkit.Rows("1"))
}

func TestJoinCast(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	var result *testkit.Result

	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t(c1 int)")
	tk.MustExec("create table t1(c1 int unsigned)")
	tk.MustExec("insert into t values (1)")
	tk.MustExec("insert into t1 values (1)")
	result = tk.MustQuery("select t.c1 from t , t1 where t.c1 = t1.c1")
	result.Check(testkit.Rows("1"))

	// int64(-1) != uint64(18446744073709551615)
	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t(c1 bigint)")
	tk.MustExec("create table t1(c1 bigint unsigned)")
	tk.MustExec("insert into t values (-1)")
	tk.MustExec("insert into t1 values (18446744073709551615)")
	result = tk.MustQuery("select * from t , t1 where t.c1 = t1.c1")
	result.Check(testkit.Rows())

	// float(1) == double(1)
	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t(c1 float)")
	tk.MustExec("create table t1(c1 double)")
	tk.MustExec("insert into t values (1.0)")
	tk.MustExec("insert into t1 values (1.00)")
	result = tk.MustQuery("select t.c1 from t , t1 where t.c1 = t1.c1")
	result.Check(testkit.Rows("1"))

	// varchar("x") == char("x")
	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t(c1 varchar(1))")
	tk.MustExec("create table t1(c1 char(1))")
	tk.MustExec(`insert into t values ("x")`)
	tk.MustExec(`insert into t1 values ("x")`)
	result = tk.MustQuery("select t.c1 from t , t1 where t.c1 = t1.c1")
	result.Check(testkit.Rows("x"))

	// varchar("x") != char("y")
	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t(c1 varchar(1))")
	tk.MustExec("create table t1(c1 char(1))")
	tk.MustExec(`insert into t values ("x")`)
	tk.MustExec(`insert into t1 values ("y")`)
	result = tk.MustQuery("select t.c1 from t , t1 where t.c1 = t1.c1")
	result.Check(testkit.Rows())

	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t(c1 int,c2 double)")
	tk.MustExec("create table t1(c1 double,c2 int)")
	tk.MustExec("insert into t values (1, 2), (1, NULL)")
	tk.MustExec("insert into t1 values (1, 2), (1, NULL)")
	result = tk.MustQuery("select * from t a , t1 b where (a.c1, a.c2) = (b.c1, b.c2);")
	result.Check(testkit.Rows("1 2 1 2"))

	/* Issue 11895 */
	tk.MustExec("drop table if exists t;")
	tk.MustExec("drop table if exists t1;")
	tk.MustExec("create table t(c1 bigint unsigned);")
	tk.MustExec("create table t1(c1 bit(64));")
	tk.MustExec("insert into t value(18446744073709551615);")
	tk.MustExec("insert into t1 value(-1);")
	result = tk.MustQuery("select * from t, t1 where t.c1 = t1.c1;")
	require.Len(t, result.Rows(), 1)

	/* Issues 11896 */
	tk.MustExec("drop table if exists t;")
	tk.MustExec("drop table if exists t1;")
	tk.MustExec("create table t(c1 bigint);")
	tk.MustExec("create table t1(c1 bit(64));")
	tk.MustExec("insert into t value(1);")
	tk.MustExec("insert into t1 value(1);")
	result = tk.MustQuery("select * from t, t1 where t.c1 = t1.c1;")
	require.Len(t, result.Rows(), 1)

	tk.MustExec("drop table if exists t;")
	tk.MustExec("drop table if exists t1;")
	tk.MustExec("create table t(c1 bigint);")
	tk.MustExec("create table t1(c1 bit(64));")
	tk.MustExec("insert into t value(-1);")
	tk.MustExec("insert into t1 value(18446744073709551615);")
	result = tk.MustQuery("select * from t, t1 where t.c1 = t1.c1;")
	// TODO: MySQL will return one row, because c1 in t1 is 0xffffffff, which equals to -1.
	require.Len(t, result.Rows(), 0)

	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("drop table if exists t2")
	tk.MustExec("create table t(c1 bigint)")
	tk.MustExec("create table t1(c1 bigint unsigned)")
	tk.MustExec("create table t2(c1 Date)")
	tk.MustExec("insert into t value(20191111)")
	tk.MustExec("insert into t1 value(20191111)")
	tk.MustExec("insert into t2 value('2019-11-11')")
	result = tk.MustQuery("select * from t, t1, t2 where t.c1 = t2.c1 and t1.c1 = t2.c1")
	result.Check(testkit.Rows("20191111 20191111 2019-11-11"))

	tk.MustExec("drop table if exists t;")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("drop table if exists t2;")
	tk.MustExec("create table t(c1 bigint);")
	tk.MustExec("create table t1(c1 bigint unsigned);")
	tk.MustExec("create table t2(c1 enum('a', 'b', 'c', 'd'));")
	tk.MustExec("insert into t value(3);")
	tk.MustExec("insert into t1 value(3);")
	tk.MustExec("insert into t2 value('c');")
	result = tk.MustQuery("select * from t, t1, t2 where t.c1 = t2.c1 and t1.c1 = t2.c1;")
	result.Check(testkit.Rows("3 3 c"))

	tk.MustExec("drop table if exists t;")
	tk.MustExec("drop table if exists t1;")
	tk.MustExec("drop table if exists t2;")
	tk.MustExec("create table t(c1 bigint);")
	tk.MustExec("create table t1(c1 bigint unsigned);")
	tk.MustExec("create table t2 (c1 SET('a', 'b', 'c', 'd'));")
	tk.MustExec("insert into t value(9);")
	tk.MustExec("insert into t1 value(9);")
	tk.MustExec("insert into t2 value('a,d');")
	result = tk.MustQuery("select * from t, t1, t2 where t.c1 = t2.c1 and t1.c1 = t2.c1;")
	result.Check(testkit.Rows("9 9 a,d"))

	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t(c1 int)")
	tk.MustExec("create table t1(c1 decimal(4,2))")
	tk.MustExec("insert into t values(0), (2)")
	tk.MustExec("insert into t1 values(0), (9)")
	result = tk.MustQuery("select * from t left join t1 on t1.c1 = t.c1")
	result.Sort().Check(testkit.Rows("0 0.00", "2 <nil>"))

	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t(c1 decimal(4,1))")
	tk.MustExec("create table t1(c1 decimal(4,2))")
	tk.MustExec("insert into t values(0), (2)")
	tk.MustExec("insert into t1 values(0), (9)")
	result = tk.MustQuery("select * from t left join t1 on t1.c1 = t.c1")
	result.Sort().Check(testkit.Rows("0.0 0.00", "2.0 <nil>"))

	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t(c1 decimal(4,1))")
	tk.MustExec("create table t1(c1 decimal(4,2))")
	tk.MustExec("create index k1 on t1(c1)")
	tk.MustExec("insert into t values(0), (2)")
	tk.MustExec("insert into t1 values(0), (9)")
	result = tk.MustQuery("select /*+ INL_JOIN(t1) */ * from t left join t1 on t1.c1 = t.c1")
	result.Sort().Check(testkit.Rows("0.0 0.00", "2.0 <nil>"))
	result = tk.MustQuery("select /*+ INL_HASH_JOIN(t1) */ * from t left join t1 on t1.c1 = t.c1")
	result.Sort().Check(testkit.Rows("0.0 0.00", "2.0 <nil>"))
	result = tk.MustQuery("select /*+ INL_MERGE_JOIN(t1) */ * from t left join t1 on t1.c1 = t.c1")
	result.Sort().Check(testkit.Rows("0.0 0.00", "2.0 <nil>"))

	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("drop table if exists t2")
	tk.MustExec("create table t(c1 char(10))")
	tk.MustExec("create table t1(c1 char(10))")
	tk.MustExec("create table t2(c1 char(10))")
	tk.MustExec("insert into t values('abd')")
	tk.MustExec("insert into t1 values('abc')")
	tk.MustExec("insert into t2 values('abc')")
	result = tk.MustQuery("select * from (select * from t union all select * from t1) t1 join t2 on t1.c1 = t2.c1")
	result.Sort().Check(testkit.Rows("abc abc"))

	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a varchar(10), index idx(a))")
	tk.MustExec("insert into t values('1'), ('2'), ('3')")
	tk.MustExec("set @@tidb_init_chunk_size=1")
	result = tk.MustQuery("select a from (select /*+ INL_JOIN(t1, t2) */ t1.a from t t1 join t t2 on t1.a=t2.a) t group by a")
	result.Sort().Check(testkit.Rows("1", "2", "3"))
	result = tk.MustQuery("select a from (select /*+ INL_HASH_JOIN(t1, t2) */ t1.a from t t1 join t t2 on t1.a=t2.a) t group by a")
	result.Sort().Check(testkit.Rows("1", "2", "3"))
	result = tk.MustQuery("select a from (select /*+ INL_MERGE_JOIN(t1, t2) */ t1.a from t t1 join t t2 on t1.a=t2.a) t group by a")
	result.Sort().Check(testkit.Rows("1", "2", "3"))
	tk.MustExec("set @@tidb_init_chunk_size=32")
}

func TestUsing(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)

	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1, t2, t3, t4")
	tk.MustExec("create table t1 (a int, c int)")
	tk.MustExec("create table t2 (a int, d int)")
	tk.MustExec("create table t3 (a int)")
	tk.MustExec("create table t4 (a int)")
	tk.MustExec("insert t1 values (2, 4), (1, 3)")
	tk.MustExec("insert t2 values (2, 5), (3, 6)")
	tk.MustExec("insert t3 values (1)")

	tk.MustQuery("select * from t1 join t2 using (a)").Check(testkit.Rows("2 4 5"))
	tk.MustQuery("select t1.a, t2.a from t1 join t2 using (a)").Check(testkit.Rows("2 2"))

	tk.MustQuery("select * from t1 right join t2 using (a) order by a").Check(testkit.Rows("2 5 4", "3 6 <nil>"))
	tk.MustQuery("select t1.a, t2.a from t1 right join t2 using (a) order by t2.a").Check(testkit.Rows("2 2", "<nil> 3"))

	tk.MustQuery("select * from t1 left join t2 using (a) order by a").Check(testkit.Rows("1 3 <nil>", "2 4 5"))
	tk.MustQuery("select t1.a, t2.a from t1 left join t2 using (a) order by t1.a").Check(testkit.Rows("1 <nil>", "2 2"))

	tk.MustQuery("select * from t1 join t2 using (a) right join t3 using (a)").Check(testkit.Rows("1 <nil> <nil>"))
	tk.MustQuery("select * from t1 join t2 using (a) right join t3 on (t2.a = t3.a)").Check(testkit.Rows("<nil> <nil> <nil> 1"))
	tk.MustQuery("select t2.a from t1 join t2 using (a) right join t3 on (t1.a = t3.a)").Check(testkit.Rows("<nil>"))
	tk.MustQuery("select t1.a, t2.a, t3.a from t1 join t2 using (a) right join t3 using (a)").Check(testkit.Rows("<nil> <nil> 1"))
	tk.MustQuery("select t1.c, t2.d from t1 join t2 using (a) right join t3 using (a)").Check(testkit.Rows("<nil> <nil>"))

	tk.MustExec("alter table t1 add column b int default 1 after a")
	tk.MustExec("alter table t2 add column b int default 1 after a")
	tk.MustQuery("select * from t1 join t2 using (b, a)").Check(testkit.Rows("2 1 4 5"))

	tk.MustExec("select * from (t1 join t2 using (a)) join (t3 join t4 using (a)) on (t2.a = t4.a and t1.a = t3.a)")

	tk.MustExec("drop table if exists t, tt")
	tk.MustExec("create table t(a int, b int)")
	tk.MustExec("create table tt(b int, a int)")
	tk.MustExec("insert into t (a, b) values(1, 1)")
	tk.MustExec("insert into tt (a, b) values(1, 2)")
	tk.MustQuery("select * from t join tt using(a)").Check(testkit.Rows("1 1 2"))

	tk.MustExec("drop table if exists t, tt")
	tk.MustExec("create table t(a float, b int)")
	tk.MustExec("create table tt(b bigint, a int)")
	// Check whether this sql can execute successfully.
	tk.MustExec("select * from t join tt using(a)")

	tk.MustExec("drop table if exists t, s")
	tk.MustExec("create table t(a int, b int)")
	tk.MustExec("create table s(b int, a int)")
	tk.MustExec("insert into t values(1,1), (2,2), (3,3), (null,null)")
	tk.MustExec("insert into s values(1,1), (3,3), (null,null)")

	// For issue 20477
	tk.MustQuery("select t.*, s.* from t join s using(a)").Check(testkit.Rows("1 1 1 1", "3 3 3 3"))
	tk.MustQuery("select s.a from t join s using(a)").Check(testkit.Rows("1", "3"))
	tk.MustQuery("select s.a from t join s using(a) where s.a > 1").Check(testkit.Rows("3"))
	tk.MustQuery("select s.a from t join s using(a) order by s.a").Check(testkit.Rows("1", "3"))
	tk.MustQuery("select s.a from t join s using(a) where s.a > 1 order by s.a").Check(testkit.Rows("3"))
	tk.MustQuery("select s.a from t join s using(a) where s.a > 1 order by s.a limit 2").Check(testkit.Rows("3"))

	// For issue 20441
	tk.MustExec(`DROP TABLE if exists t1, t2, t3`)
	tk.MustExec(`create table t1 (i int)`)
	tk.MustExec(`create table t2 (i int)`)
	tk.MustExec(`create table t3 (i int)`)
	tk.MustExec(`select * from t1,t2 natural left join t3 order by t1.i,t2.i,t3.i`)
	tk.MustExec(`select t1.i,t2.i,t3.i from t2 natural left join t3,t1 order by t1.i,t2.i,t3.i`)
	tk.MustExec(`select * from t1,t2 natural right join t3 order by t1.i,t2.i,t3.i`)
	tk.MustExec(`select t1.i,t2.i,t3.i from t2 natural right join t3,t1 order by t1.i,t2.i,t3.i`)

	// For issue 15844
	tk.MustExec(`DROP TABLE if exists t0, t1`)
	tk.MustExec(`CREATE TABLE t0(c0 INT)`)
	tk.MustExec(`CREATE TABLE t1(c0 INT)`)
	tk.MustExec(`SELECT t0.c0 FROM t0 NATURAL RIGHT JOIN t1 WHERE t1.c0`)

	// For issue 20958
	tk.MustExec(`DROP TABLE if exists t1, t2`)
	tk.MustExec(`create table t1(id int, name varchar(20));`)
	tk.MustExec(`create table t2(id int, address varchar(30));`)
	tk.MustExec(`insert into t1 values(1,'gangshen');`)
	tk.MustExec(`insert into t2 values(1,'HangZhou');`)
	tk.MustQuery(`select t2.* from t1 inner join t2 using (id) limit 1;`).Check(testkit.Rows("1 HangZhou"))
	tk.MustQuery(`select t2.* from t1 inner join t2 on t1.id = t2.id  limit 1;`).Check(testkit.Rows("1 HangZhou"))

	// For issue 20476
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t1(a int)")
	tk.MustExec("insert into t1 (a) values(1)")
	tk.MustQuery("select t1.*, t2.* from t1 join t1 t2 using(a)").Check(testkit.Rows("1 1"))
	tk.MustQuery("select * from t1 join t1 t2 using(a)").Check(testkit.Rows("1"))

	// For issue 18992
	tk.MustExec("drop table t")
	tk.MustExec("CREATE TABLE t (   a varchar(55) NOT NULL,   b varchar(55) NOT NULL,   c int(11) DEFAULT NULL,   d int(11) DEFAULT NULL ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;")
	tk.MustExec("update t t1 join t t2 using(a,b) set t1.c=t2.d;")

	// For issue 20467
	tk.MustExec(`DROP TABLE if exists t1,t2,t3,t4,t5`)
	tk.MustExec(`CREATE TABLE t1 (a INT, b INT)`)
	tk.MustExec(`CREATE TABLE t2 (a INT, b INT)`)
	tk.MustExec(`CREATE TABLE t3 (a INT, b INT)`)
	tk.MustExec(`INSERT INTO t1 VALUES (1,1)`)
	tk.MustExec(`INSERT INTO t2 VALUES (1,1)`)
	tk.MustExec(`INSERT INTO t3 VALUES (1,1)`)
	tk.MustGetErrMsg(`SELECT * FROM t1 JOIN (t2 JOIN t3 USING (b)) USING (a)`, "[planner:1052]Column 'a' in from clause is ambiguous")

	// For issue 6712
	tk.MustExec("drop table if exists t1,t2")
	tk.MustExec("create table t1 (t1 int , t0 int)")
	tk.MustExec("create table t2 (t2 int, t0 int)")
	tk.MustExec("insert into t1 select 11, 1")
	tk.MustExec("insert into t2 select 22, 1")
	tk.MustQuery("select t1.t0, t2.t0 from t1 join t2 using(t0) group by t1.t0").Check(testkit.Rows("1 1"))
	tk.MustQuery("select t1.t0, t2.t0 from t1 join t2 using(t0) having t1.t0 > 0").Check(testkit.Rows("1 1"))
}

func TestUsingAndNaturalJoinSchema(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1, t2, t3, t4")
	tk.MustExec("create table t1 (c int, b int);")
	tk.MustExec("create table t2 (a int, b int);")
	tk.MustExec("create table t3 (b int, c int);")
	tk.MustExec("create table t4 (y int, c int);")

	tk.MustExec("insert into t1 values (10,1);")
	tk.MustExec("insert into t1 values (3 ,1);")
	tk.MustExec("insert into t1 values (3 ,2);")
	tk.MustExec("insert into t2 values (2, 1);")
	tk.MustExec("insert into t3 values (1, 3);")
	tk.MustExec("insert into t3 values (1,10);")
	tk.MustExec("insert into t4 values (11,3);")
	tk.MustExec("insert into t4 values (2, 3);")

	var input []string
	var output []struct {
		SQL string
		Res []string
	}
	executorSuiteData.LoadTestCases(t, &input, &output)
	for i, tt := range input {
		testdata.OnRecord(func() {
			output[i].SQL = tt
			output[i].Res = testdata.ConvertRowsToStrings(tk.MustQuery(tt).Sort().Rows())
		})
		tk.MustQuery(tt).Sort().Check(testkit.Rows(output[i].Res...))
	}
}

func TestNaturalJoin(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)

	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1, t2")
	tk.MustExec("create table t1 (a int, b int)")
	tk.MustExec("create table t2 (a int, c int)")
	tk.MustExec("insert t1 values (1,2), (10,20), (0,0)")
	tk.MustExec("insert t2 values (1,3), (100,200), (0,0)")

	var input []string
	var output []struct {
		SQL  string
		Plan []string
		Res  []string
	}
	executorSuiteData.LoadTestCases(t, &input, &output)
	for i, tt := range input {
		testdata.OnRecord(func() {
			output[i].SQL = tt
			output[i].Plan = testdata.ConvertRowsToStrings(tk.MustQuery("explain format = 'brief' " + tt).Rows())
			output[i].Res = testdata.ConvertRowsToStrings(tk.MustQuery(tt).Sort().Rows())
		})
		tk.MustQuery("explain format = 'brief' " + tt).Check(testkit.Rows(output[i].Plan...))
		tk.MustQuery(tt).Sort().Check(testkit.Rows(output[i].Res...))
	}
}

func TestMultiJoin(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table t35(a35 int primary key, b35 int, x35 int)")
	tk.MustExec("create table t40(a40 int primary key, b40 int, x40 int)")
	tk.MustExec("create table t14(a14 int primary key, b14 int, x14 int)")
	tk.MustExec("create table t42(a42 int primary key, b42 int, x42 int)")
	tk.MustExec("create table t15(a15 int primary key, b15 int, x15 int)")
	tk.MustExec("create table t7(a7 int primary key, b7 int, x7 int)")
	tk.MustExec("create table t64(a64 int primary key, b64 int, x64 int)")
	tk.MustExec("create table t19(a19 int primary key, b19 int, x19 int)")
	tk.MustExec("create table t9(a9 int primary key, b9 int, x9 int)")
	tk.MustExec("create table t8(a8 int primary key, b8 int, x8 int)")
	tk.MustExec("create table t57(a57 int primary key, b57 int, x57 int)")
	tk.MustExec("create table t37(a37 int primary key, b37 int, x37 int)")
	tk.MustExec("create table t44(a44 int primary key, b44 int, x44 int)")
	tk.MustExec("create table t38(a38 int primary key, b38 int, x38 int)")
	tk.MustExec("create table t18(a18 int primary key, b18 int, x18 int)")
	tk.MustExec("create table t62(a62 int primary key, b62 int, x62 int)")
	tk.MustExec("create table t4(a4 int primary key, b4 int, x4 int)")
	tk.MustExec("create table t48(a48 int primary key, b48 int, x48 int)")
	tk.MustExec("create table t31(a31 int primary key, b31 int, x31 int)")
	tk.MustExec("create table t16(a16 int primary key, b16 int, x16 int)")
	tk.MustExec("create table t12(a12 int primary key, b12 int, x12 int)")
	tk.MustExec("insert into t35 values(1,1,1)")
	tk.MustExec("insert into t40 values(1,1,1)")
	tk.MustExec("insert into t14 values(1,1,1)")
	tk.MustExec("insert into t42 values(1,1,1)")
	tk.MustExec("insert into t15 values(1,1,1)")
	tk.MustExec("insert into t7 values(1,1,1)")
	tk.MustExec("insert into t64 values(1,1,1)")
	tk.MustExec("insert into t19 values(1,1,1)")
	tk.MustExec("insert into t9 values(1,1,1)")
	tk.MustExec("insert into t8 values(1,1,1)")
	tk.MustExec("insert into t57 values(1,1,1)")
	tk.MustExec("insert into t37 values(1,1,1)")
	tk.MustExec("insert into t44 values(1,1,1)")
	tk.MustExec("insert into t38 values(1,1,1)")
	tk.MustExec("insert into t18 values(1,1,1)")
	tk.MustExec("insert into t62 values(1,1,1)")
	tk.MustExec("insert into t4 values(1,1,1)")
	tk.MustExec("insert into t48 values(1,1,1)")
	tk.MustExec("insert into t31 values(1,1,1)")
	tk.MustExec("insert into t16 values(1,1,1)")
	tk.MustExec("insert into t12 values(1,1,1)")
	tk.MustExec("insert into t35 values(7,7,7)")
	tk.MustExec("insert into t40 values(7,7,7)")
	tk.MustExec("insert into t14 values(7,7,7)")
	tk.MustExec("insert into t42 values(7,7,7)")
	tk.MustExec("insert into t15 values(7,7,7)")
	tk.MustExec("insert into t7 values(7,7,7)")
	tk.MustExec("insert into t64 values(7,7,7)")
	tk.MustExec("insert into t19 values(7,7,7)")
	tk.MustExec("insert into t9 values(7,7,7)")
	tk.MustExec("insert into t8 values(7,7,7)")
	tk.MustExec("insert into t57 values(7,7,7)")
	tk.MustExec("insert into t37 values(7,7,7)")
	tk.MustExec("insert into t44 values(7,7,7)")
	tk.MustExec("insert into t38 values(7,7,7)")
	tk.MustExec("insert into t18 values(7,7,7)")
	tk.MustExec("insert into t62 values(7,7,7)")
	tk.MustExec("insert into t4 values(7,7,7)")
	tk.MustExec("insert into t48 values(7,7,7)")
	tk.MustExec("insert into t31 values(7,7,7)")
	tk.MustExec("insert into t16 values(7,7,7)")
	tk.MustExec("insert into t12 values(7,7,7)")
	result := tk.MustQuery(`SELECT x4,x8,x38,x44,x31,x9,x57,x48,x19,x40,x14,x12,x7,x64,x37,x18,x62,x35,x42,x15,x16 FROM
t35,t40,t14,t42,t15,t7,t64,t19,t9,t8,t57,t37,t44,t38,t18,t62,t4,t48,t31,t16,t12
WHERE b48=a57
AND a4=b19
AND a14=b16
AND b37=a48
AND a40=b42
AND a31=7
AND a15=b40
AND a38=b8
AND b15=a31
AND b64=a18
AND b12=a44
AND b7=a8
AND b35=a16
AND a12=b14
AND a64=b57
AND b62=a7
AND a35=b38
AND b9=a19
AND a62=b18
AND b4=a37
AND b44=a42`)
	result.Check(testkit.Rows("7 7 7 7 7 7 7 7 7 7 7 7 7 7 7 7 7 7 7 7 7"))
}

func TestSubquerySameTable(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t (a int)")
	tk.MustExec("insert t values (1), (2)")
	result := tk.MustQuery("select a from t where exists(select 1 from t as x where x.a < t.a)")
	result.Check(testkit.Rows("2"))
	result = tk.MustQuery("select a from t where not exists(select 1 from t as x where x.a < t.a)")
	result.Check(testkit.Rows("1"))
}

func TestSubquery(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("set @@tidb_hash_join_concurrency=1")
	tk.MustExec("set @@tidb_hashagg_partial_concurrency=1")
	tk.MustExec("set @@tidb_hashagg_final_concurrency=1")
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t (c int, d int)")
	tk.MustExec("insert t values (1, 1)")
	tk.MustExec("insert t values (2, 2)")
	tk.MustExec("insert t values (3, 4)")
	tk.MustExec("commit")

	tk.MustExec("set sql_mode = 'STRICT_TRANS_TABLES'")

	result := tk.MustQuery("select * from t where exists(select * from t k where t.c = k.c having sum(c) = 1)")
	result.Check(testkit.Rows("1 1"))
	result = tk.MustQuery("select * from t where exists(select k.c, k.d from t k, t p where t.c = k.d)")
	result.Check(testkit.Rows("1 1", "2 2"))
	result = tk.MustQuery("select 1 = (select count(*) from t where t.c = k.d) from t k")
	result.Check(testkit.Rows("1", "1", "0"))
	result = tk.MustQuery("select 1 = (select count(*) from t where exists( select * from t m where t.c = k.d)) from t k")
	result.Sort().Check(testkit.Rows("0", "1", "1"))
	result = tk.MustQuery("select t.c = any (select count(*) from t) from t")
	result.Sort().Check(testkit.Rows("0", "0", "1"))
	result = tk.MustQuery("select * from t where (t.c, 6) = any (select count(*), sum(t.c) from t)")
	result.Check(testkit.Rows("3 4"))
	result = tk.MustQuery("select t.c from t where (t.c) < all (select count(*) from t)")
	result.Check(testkit.Rows("1", "2"))
	result = tk.MustQuery("select t.c from t where (t.c, t.d) = any (select * from t)")
	result.Sort().Check(testkit.Rows("1", "2", "3"))
	result = tk.MustQuery("select t.c from t where (t.c, t.d) != all (select * from t)")
	result.Check(testkit.Rows())
	result = tk.MustQuery("select (select count(*) from t where t.c = k.d) from t k")
	result.Sort().Check(testkit.Rows("0", "1", "1"))
	result = tk.MustQuery("select t.c from t where (t.c, t.d) in (select * from t)")
	result.Sort().Check(testkit.Rows("1", "2", "3"))
	result = tk.MustQuery("select t.c from t where (t.c, t.d) not in (select * from t)")
	result.Check(testkit.Rows())
	result = tk.MustQuery("select * from t A inner join t B on A.c = B.c and A.c > 100")
	result.Check(testkit.Rows())
	// = all empty set is true
	result = tk.MustQuery("select t.c from t where (t.c, t.d) != all (select * from t where d > 1000)")
	result.Sort().Check(testkit.Rows("1", "2", "3"))
	result = tk.MustQuery("select t.c from t where (t.c) < any (select c from t where d > 1000)")
	result.Check(testkit.Rows())
	tk.MustExec("insert t values (NULL, NULL)")
	result = tk.MustQuery("select (t.c) < any (select c from t) from t")
	result.Sort().Check(testkit.Rows("1", "1", "<nil>", "<nil>"))
	result = tk.MustQuery("select (10) > all (select c from t) from t")
	result.Check(testkit.Rows("<nil>", "<nil>", "<nil>", "<nil>"))
	result = tk.MustQuery("select (c) > all (select c from t) from t")
	result.Check(testkit.Rows("0", "0", "0", "<nil>"))

	tk.MustExec("drop table if exists a")
	tk.MustExec("create table a (c int, d int)")
	tk.MustExec("insert a values (1, 2)")
	tk.MustExec("drop table if exists b")
	tk.MustExec("create table b (c int, d int)")
	tk.MustExec("insert b values (2, 1)")

	result = tk.MustQuery("select * from a b where c = (select d from b a where a.c = 2 and b.c = 1)")
	result.Check(testkit.Rows("1 2"))

	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(c int)")
	tk.MustExec("insert t values(10), (8), (7), (9), (11)")
	result = tk.MustQuery("select * from t where 9 in (select c from t s where s.c < t.c limit 3)")
	result.Check(testkit.Rows("10"))

	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(id int, v int)")
	tk.MustExec("insert into t values(1, 1), (2, 2), (3, 3)")
	result = tk.MustQuery("select * from t where v=(select min(t1.v) from t t1, t t2, t t3 where t1.id=t2.id and t2.id=t3.id and t1.id=t.id)")
	result.Check(testkit.Rows("1 1", "2 2", "3 3"))

	result = tk.MustQuery("select exists (select t.id from t where s.id < 2 and t.id = s.id) from t s")
	result.Sort().Check(testkit.Rows("0", "0", "1"))

	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(c int)")
	result = tk.MustQuery("select exists(select count(*) from t)")
	result.Check(testkit.Rows("1"))

	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(id int primary key, v int)")
	tk.MustExec("insert into t values(1, 1), (2, 2), (3, 3)")
	result = tk.MustQuery("select (select t.id from t where s.id < 2 and t.id = s.id) from t s")
	result.Sort().Check(testkit.Rows("1", "<nil>", "<nil>"))
	rs, err := tk.Exec("select (select t.id from t where t.id = t.v and t.v != s.id) from t s")
	require.NoError(t, err)
	_, err = session.GetRows4Test(context.Background(), tk.Session(), rs)
	require.Error(t, err)
	require.NoError(t, rs.Close())

	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists s")
	tk.MustExec("create table t(id int)")
	tk.MustExec("create table s(id int)")
	tk.MustExec("insert into t values(1), (2)")
	tk.MustExec("insert into s values(2), (2)")
	result = tk.MustQuery("select id from t where(select count(*) from s where s.id = t.id) > 0")
	result.Check(testkit.Rows("2"))
	result = tk.MustQuery("select *, (select count(*) from s where id = t.id limit 1, 1) from t")
	result.Check(testkit.Rows("1 <nil>", "2 <nil>"))

	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists s")
	tk.MustExec("create table t(id int primary key)")
	tk.MustExec("create table s(id int)")
	tk.MustExec("insert into t values(1), (2)")
	tk.MustExec("insert into s values(2), (2)")
	result = tk.MustQuery("select *, (select count(id) from s where id = t.id) from t")
	result.Check(testkit.Rows("1 0", "2 2"))
	result = tk.MustQuery("select *, 0 < any (select count(id) from s where id = t.id) from t")
	result.Check(testkit.Rows("1 0", "2 1"))
	result = tk.MustQuery("select (select count(*) from t k where t.id = id) from s, t where t.id = s.id limit 1")
	result.Check(testkit.Rows("1"))

	tk.MustExec("drop table if exists t, s")
	tk.MustExec("create table t(id int primary key)")
	tk.MustExec("create table s(id int, index k(id))")
	tk.MustExec("insert into t values(1), (2)")
	tk.MustExec("insert into s values(2), (2)")
	result = tk.MustQuery("select (select id from s where s.id = t.id order by s.id limit 1) from t")
	result.Check(testkit.Rows("<nil>", "2"))

	tk.MustExec("drop table if exists t, s")
	tk.MustExec("create table t(id int)")
	tk.MustExec("create table s(id int)")
	tk.MustExec("insert into t values(2), (2)")
	tk.MustExec("insert into s values(2)")
	result = tk.MustQuery("select (select id from s where s.id = t.id order by s.id) from t")
	result.Check(testkit.Rows("2", "2"))

	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(dt datetime)")
	result = tk.MustQuery("select (select 1 from t where DATE_FORMAT(o.dt,'%Y-%m')) from t o")
	result.Check(testkit.Rows())

	tk.MustExec("drop table if exists t1, t2")
	tk.MustExec("create table t1(f1 int, f2 int)")
	tk.MustExec("create table t2(fa int, fb int)")
	tk.MustExec("insert into t1 values (1,1),(1,1),(1,2),(1,2),(1,2),(1,3)")
	tk.MustExec("insert into t2 values (1,1),(1,2),(1,3)")
	result = tk.MustQuery("select f1,f2 from t1 group by f1,f2 having count(1) >= all (select fb from t2 where fa = f1)")
	result.Check(testkit.Rows("1 2"))

	tk.MustExec("DROP TABLE IF EXISTS t1, t2")
	tk.MustExec("CREATE TABLE t1(a INT)")
	tk.MustExec("CREATE TABLE t2 (d BINARY(2), PRIMARY KEY (d(1)), UNIQUE KEY (d))")
	tk.MustExec("INSERT INTO t1 values(1)")
	result = tk.MustQuery("SELECT 1 FROM test.t1, test.t2 WHERE 1 = (SELECT test.t2.d FROM test.t2 WHERE test.t1.a >= 1) and test.t2.d = 1;")
	result.Check(testkit.Rows())

	tk.MustExec("DROP TABLE IF EXISTS t1")
	tk.MustExec("CREATE TABLE t1(a int, b int default 0)")
	tk.MustExec("create index k1 on t1(a)")
	tk.MustExec("INSERT INTO t1 (a) values(1), (2), (3), (4), (5)")
	result = tk.MustQuery("select (select /*+ INL_JOIN(x2) */ x2.a from t1 x1, t1 x2 where x1.a = t1.a and x1.a = x2.a) from t1")
	result.Check(testkit.Rows("1", "2", "3", "4", "5"))
	result = tk.MustQuery("select (select /*+ INL_HASH_JOIN(x2) */ x2.a from t1 x1, t1 x2 where x1.a = t1.a and x1.a = x2.a) from t1")
	result.Check(testkit.Rows("1", "2", "3", "4", "5"))
	result = tk.MustQuery("select (select /*+ INL_MERGE_JOIN(x2) */ x2.a from t1 x1, t1 x2 where x1.a = t1.a and x1.a = x2.a) from t1")
	result.Check(testkit.Rows("1", "2", "3", "4", "5"))

	// test left outer semi join & anti left outer semi join
	tk.MustQuery("select 1 from (select t1.a in (select t1.a from t1) from t1) x;").Check(testkit.Rows("1", "1", "1", "1", "1"))
	tk.MustQuery("select 1 from (select t1.a not in (select t1.a from t1) from t1) x;").Check(testkit.Rows("1", "1", "1", "1", "1"))

	tk.MustExec("drop table if exists t1, t2")
	tk.MustExec("create table t1(a int)")
	tk.MustExec("create table t2(b int)")
	tk.MustExec("insert into t1 values(1)")
	tk.MustExec("insert into t2 values(1)")
	tk.MustQuery("select * from t1 where a in (select a from t2)").Check(testkit.Rows("1"))

	tk.MustExec("insert into t2 value(null)")
	tk.MustQuery("select * from t1 where 1 in (select b from t2)").Check(testkit.Rows("1"))
	tk.MustQuery("select * from t1 where 1 not in (select b from t2)").Check(testkit.Rows())
	tk.MustQuery("select * from t1 where 2 not in (select b from t2)").Check(testkit.Rows())
	tk.MustQuery("select * from t1 where 2 in (select b from t2)").Check(testkit.Rows())
	tk.MustQuery("select 1 in (select b from t2) from t1").Check(testkit.Rows("1"))
	tk.MustQuery("select 1 in (select 1 from t2) from t1").Check(testkit.Rows("1"))
	tk.MustQuery("select 1 not in (select b from t2) from t1").Check(testkit.Rows("0"))
	tk.MustQuery("select 1 not in (select 1 from t2) from t1").Check(testkit.Rows("0"))

	tk.MustExec("delete from t2 where b=1")
	tk.MustQuery("select 1 in (select b from t2) from t1").Check(testkit.Rows("<nil>"))
	tk.MustQuery("select 1 not in (select b from t2) from t1").Check(testkit.Rows("<nil>"))
	tk.MustQuery("select 1 not in (select 1 from t2) from t1").Check(testkit.Rows("0"))
	tk.MustQuery("select 1 in (select 1 from t2) from t1").Check(testkit.Rows("1"))
	tk.MustQuery("select 1 not in (select null from t1) from t2").Check(testkit.Rows("<nil>"))
	tk.MustQuery("select 1 in (select null from t1) from t2").Check(testkit.Rows("<nil>"))

	tk.MustExec("drop table if exists s")
	tk.MustExec("create table s(a int not null, b int)")
	tk.MustExec("set sql_mode = ''")
	tk.MustQuery("select (2,0) in (select s.a, min(s.b) from s) as f").Check(testkit.Rows("<nil>"))
	tk.MustQuery("select (2,0) not in (select s.a, min(s.b) from s) as f").Check(testkit.Rows("<nil>"))
	tk.MustQuery("select (2,0) = any (select s.a, min(s.b) from s) as f").Check(testkit.Rows("<nil>"))
	tk.MustQuery("select (2,0) != all (select s.a, min(s.b) from s) as f").Check(testkit.Rows("<nil>"))
	tk.MustQuery("select (2,0) in (select s.b, min(s.b) from s) as f").Check(testkit.Rows("<nil>"))
	tk.MustQuery("select (2,0) not in (select s.b, min(s.b) from s) as f").Check(testkit.Rows("<nil>"))
	tk.MustQuery("select (2,0) = any (select s.b, min(s.b) from s) as f").Check(testkit.Rows("<nil>"))
	tk.MustQuery("select (2,0) != all (select s.b, min(s.b) from s) as f").Check(testkit.Rows("<nil>"))
	tk.MustExec("insert into s values(1,null)")
	tk.MustQuery("select 1 in (select b from s)").Check(testkit.Rows("<nil>"))

	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int)")
	tk.MustExec("insert into t values(1),(null)")
	tk.MustQuery("select a not in (select 1) from t").Sort().Check(testkit.Rows(
		"0",
		"<nil>",
	))
	tk.MustQuery("select 1 not in (select null from t t1) from t").Check(testkit.Rows(
		"<nil>",
		"<nil>",
	))
	tk.MustQuery("select 1 in (select null from t t1) from t").Check(testkit.Rows(
		"<nil>",
		"<nil>",
	))
	tk.MustQuery("select a in (select 0) xx from (select null as a) x").Check(testkit.Rows("<nil>"))

	tk.MustExec("drop table t")
	tk.MustExec("create table t(a int, b int)")
	tk.MustExec("insert into t values(1,null),(null, null),(null, 2)")
	tk.MustQuery("select * from t t1 where (2 in (select a from t t2 where (t2.b=t1.b) is null))").Check(testkit.Rows())
	tk.MustQuery("select (t2.a in (select t1.a from t t1)) is true from t t2").Sort().Check(testkit.Rows(
		"0",
		"0",
		"1",
	))

	tk.MustExec("set @@tidb_hash_join_concurrency=5")
}

func TestInSubquery(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t (a int, b int)")
	tk.MustExec("insert t values (1, 1), (2, 1)")
	result := tk.MustQuery("select m1.a from t as m1 where m1.a in (select m2.b from t as m2)")
	result.Check(testkit.Rows("1"))
	result = tk.MustQuery("select m1.a from t as m1 where (3, m1.b) not in (select * from t as m2)")
	result.Sort().Check(testkit.Rows("1", "2"))
	result = tk.MustQuery("select m1.a from t as m1 where m1.a in (select m2.b+? from t as m2)", 1)
	result.Check(testkit.Rows("2"))
	tk.MustExec(`prepare stmt1 from 'select m1.a from t as m1 where m1.a in (select m2.b+? from t as m2)'`)
	tk.MustExec("set @a = 1")
	result = tk.MustQuery(`execute stmt1 using @a;`)
	result.Check(testkit.Rows("2"))
	tk.MustExec("set @a = 0")
	result = tk.MustQuery(`execute stmt1 using @a;`)
	result.Check(testkit.Rows("1"))

	result = tk.MustQuery("select m1.a from t as m1 where m1.a in (1, 3, 5)")
	result.Check(testkit.Rows("1"))

	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t1 (a float)")
	tk.MustExec("insert t1 values (281.37)")
	tk.MustQuery("select a from t1 where (a in (select a from t1))").Check(testkit.Rows("281.37"))

	tk.MustExec("drop table if exists t1, t2")
	tk.MustExec("create table t1 (a int, b int)")
	tk.MustExec("insert into t1 values (0,0),(1,1),(2,2),(3,3),(4,4)")
	tk.MustExec("create table t2 (a int)")
	tk.MustExec("insert into t2 values (1),(2),(3),(4),(5),(6),(7),(8),(9),(10)")
	result = tk.MustQuery("select a from t1 where (1,1) in (select * from t2 s , t2 t where t1.a = s.a and s.a = t.a limit 1)")
	result.Check(testkit.Rows("1"))

	tk.MustExec("drop table if exists t1, t2")
	tk.MustExec("create table t1 (a int)")
	tk.MustExec("create table t2 (a int)")
	tk.MustExec("insert into t1 values (1),(2)")
	tk.MustExec("insert into t2 values (1),(2)")
	tk.MustExec("set @@session.tidb_opt_insubq_to_join_and_agg = 0")
	result = tk.MustQuery("select * from t1 where a in (select * from t2)")
	result.Sort().Check(testkit.Rows("1", "2"))
	result = tk.MustQuery("select * from t1 where a in (select * from t2 where false)")
	result.Check(testkit.Rows())
	result = tk.MustQuery("select * from t1 where a not in (select * from t2 where false)")
	result.Sort().Check(testkit.Rows("1", "2"))
	tk.MustExec("set @@session.tidb_opt_insubq_to_join_and_agg = 1")
	result = tk.MustQuery("select * from t1 where a in (select * from t2)")
	result.Sort().Check(testkit.Rows("1", "2"))
	result = tk.MustQuery("select * from t1 where a in (select * from t2 where false)")
	result.Check(testkit.Rows())
	result = tk.MustQuery("select * from t1 where a not in (select * from t2 where false)")
	result.Sort().Check(testkit.Rows("1", "2"))

	tk.MustExec("drop table if exists t1, t2")
	tk.MustExec("create table t1 (a int, key b (a))")
	tk.MustExec("create table t2 (a int, key b (a))")
	tk.MustExec("insert into t1 values (1),(2),(2)")
	tk.MustExec("insert into t2 values (1),(2),(2)")
	result = tk.MustQuery("select * from t1 where a in (select * from t2) order by a desc")
	result.Check(testkit.Rows("2", "2", "1"))
	result = tk.MustQuery("select * from t1 where a in (select count(*) from t2 where t1.a = t2.a) order by a desc")
	result.Check(testkit.Rows("2", "2", "1"))
}

func TestJoinLeak(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("set @@tidb_hash_join_concurrency=1")
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t (d int)")
	tk.MustExec("begin")
	for i := 0; i < 1002; i++ {
		tk.MustExec("insert t values (1)")
	}
	tk.MustExec("commit")
	result, err := tk.Exec("select * from t t1 left join (select 1) t2 on 1")
	require.NoError(t, err)
	req := result.NewChunk(nil)
	err = result.Next(context.Background(), req)
	require.NoError(t, err)
	time.Sleep(time.Millisecond)
	require.NoError(t, result.Close())

	tk.MustExec("set @@tidb_hash_join_concurrency=5")
}

func TestHashJoinExecEncodeDecodeRow(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("drop table if exists t2")
	tk.MustExec("create table t1 (id int)")
	tk.MustExec("create table t2 (id int, name varchar(255), ts timestamp)")
	tk.MustExec("insert into t1 values (1)")
	tk.MustExec("insert into t2 values (1, 'xxx', '2003-06-09 10:51:26')")
	result := tk.MustQuery("select ts from t1 inner join t2 where t2.name = 'xxx'")
	result.Check(testkit.Rows("2003-06-09 10:51:26"))
}

func TestSubqueryInJoinOn(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("drop table if exists t2")
	tk.MustExec("create table t1 (id int)")
	tk.MustExec("create table t2 (id int)")
	tk.MustExec("insert into t1 values (1)")
	tk.MustExec("insert into t2 values (1)")

	err := tk.ExecToErr("SELECT * FROM t1 JOIN t2 on (t2.id < all (SELECT 1))")
	require.Error(t, err)
}

func TestIssue5255(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1, t2")
	tk.MustExec("create table t1(a int, b date, c float, primary key(a, b))")
	tk.MustExec("create table t2(a int primary key)")
	tk.MustExec("insert into t1 values(1, '2017-11-29', 2.2)")
	tk.MustExec("insert into t2 values(1)")
	tk.MustQuery("select /*+ INL_JOIN(t1) */ * from t1 join t2 on t1.a=t2.a").Check(testkit.Rows("1 2017-11-29 2.2 1"))
	tk.MustQuery("select /*+ INL_HASH_JOIN(t1) */ * from t1 join t2 on t1.a=t2.a").Check(testkit.Rows("1 2017-11-29 2.2 1"))
	tk.MustQuery("select /*+ INL_MERGE_JOIN(t1) */ * from t1 join t2 on t1.a=t2.a").Check(testkit.Rows("1 2017-11-29 2.2 1"))
}

func TestIssue5278(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t, tt")
	tk.MustExec("create table t(a int, b int)")
	tk.MustExec("create table tt(a varchar(10), b int)")
	tk.MustExec("insert into t values(1, 1)")
	tk.MustQuery("select * from t left join tt on t.a=tt.a left join t ttt on t.a=ttt.a").Check(testkit.Rows("1 1 <nil> <nil> 1 1"))
}

func TestIssue15850JoinNullValue(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustQuery("SELECT * FROM (select null) v NATURAL LEFT JOIN (select null) v1;").Check(testkit.Rows("<nil>"))
	require.Equal(t, uint16(0), tk.Session().GetSessionVars().StmtCtx.WarningCount())

	tk.MustExec("drop table if exists t0;")
	tk.MustExec("drop view if exists v0;")
	tk.MustExec("CREATE TABLE t0(c0 TEXT);")
	tk.MustExec("CREATE VIEW v0(c0) AS SELECT NULL;")
	tk.MustQuery("SELECT /*+ HASH_JOIN(v0) */ * FROM v0 NATURAL LEFT JOIN t0;").Check(testkit.Rows("<nil>"))
	require.Equal(t, uint16(0), tk.Session().GetSessionVars().StmtCtx.WarningCount())
	tk.MustQuery("SELECT /*+ MERGE_JOIN(v0) */ * FROM v0 NATURAL LEFT JOIN t0;").Check(testkit.Rows("<nil>"))
	require.Equal(t, uint16(0), tk.Session().GetSessionVars().StmtCtx.WarningCount())
}

func TestIndexLookupJoin(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("set tidb_cost_model_version=2")
	tk.MustExec("set @@tidb_init_chunk_size=2")
	tk.MustExec("DROP TABLE IF EXISTS t")
	tk.MustExec("CREATE TABLE `t` (`a` int, pk integer auto_increment,`b` char (20),primary key (pk))")
	tk.MustExec("CREATE INDEX idx_t_a ON t(`a`)")
	tk.MustExec("CREATE INDEX idx_t_b ON t(`b`)")
	tk.MustExec("INSERT INTO t VALUES (148307968, DEFAULT, 'nndsjofmpdxvhqv') ,  (-1327693824, DEFAULT, 'pnndsjofmpdxvhqvfny') ,  (-277544960, DEFAULT, 'fpnndsjo')")

	tk.MustExec("DROP TABLE IF EXISTS s")
	tk.MustExec("CREATE TABLE `s` (`a` int, `b` char (20))")
	tk.MustExec("CREATE INDEX idx_s_a ON s(`a`)")
	tk.MustExec("INSERT INTO s VALUES (-277544960, 'fpnndsjo') ,  (2, 'kfpnndsjof') ,  (2, 'vtdiockfpn'), (-277544960, 'fpnndsjo') ,  (2, 'kfpnndsjof') ,  (6, 'ckfp')")
	tk.MustQuery("select /*+ INL_JOIN(t, s) */ t.a from t join s on t.a = s.a").Sort().Check(testkit.Rows("-277544960", "-277544960"))
	tk.MustQuery("select /*+ INL_HASH_JOIN(t, s) */ t.a from t join s on t.a = s.a").Sort().Check(testkit.Rows("-277544960", "-277544960"))
	tk.MustQuery("select /*+ INL_MERGE_JOIN(t, s) */ t.a from t join s on t.a = s.a").Sort().Check(testkit.Rows("-277544960", "-277544960"))

	tk.MustQuery("select /*+ INL_JOIN(t, s) */ t.a from t left join s on t.a = s.a").Sort().Check(testkit.Rows("-1327693824", "-277544960", "-277544960", "148307968"))
	tk.MustQuery("select /*+ INL_HASH_JOIN(t, s) */ t.a from t left join s on t.a = s.a").Sort().Check(testkit.Rows("-1327693824", "-277544960", "-277544960", "148307968"))
	tk.MustQuery("select /*+ INL_MERGE_JOIN(t, s) */ t.a from t left join s on t.a = s.a").Sort().Check(testkit.Rows("-1327693824", "-277544960", "-277544960", "148307968"))

	tk.MustQuery("select /*+ INL_JOIN(t, s) */ t.a from t left join s on t.a = s.a where t.a = -277544960").Sort().Check(testkit.Rows("-277544960", "-277544960"))
	tk.MustQuery("select /*+ INL_HASH_JOIN(t, s) */ t.a from t left join s on t.a = s.a where t.a = -277544960").Sort().Check(testkit.Rows("-277544960", "-277544960"))
	tk.MustQuery("select /*+ INL_MERGE_JOIN(t, s) */ t.a from t left join s on t.a = s.a where t.a = -277544960").Sort().Check(testkit.Rows("-277544960", "-277544960"))

	tk.MustQuery("select /*+ INL_JOIN(t, s) */ t.a from t right join s on t.a = s.a").Sort().Check(testkit.Rows("-277544960", "-277544960", "<nil>", "<nil>", "<nil>", "<nil>"))
	tk.MustQuery("select /*+ INL_HASH_JOIN(t, s) */ t.a from t right join s on t.a = s.a").Sort().Check(testkit.Rows("-277544960", "-277544960", "<nil>", "<nil>", "<nil>", "<nil>"))
	tk.MustQuery("select /*+ INL_MERGE_JOIN(t, s) */ t.a from t right join s on t.a = s.a").Sort().Check(testkit.Rows("-277544960", "-277544960", "<nil>", "<nil>", "<nil>", "<nil>"))

	tk.MustQuery("select /*+ INL_JOIN(t, s) */ t.a from t left join s on t.a = s.a order by t.a desc").Check(testkit.Rows("148307968", "-277544960", "-277544960", "-1327693824"))
	tk.MustQuery("select /*+ INL_HASH_JOIN(t, s) */ t.a from t left join s on t.a = s.a order by t.a desc").Check(testkit.Rows("148307968", "-277544960", "-277544960", "-1327693824"))
	tk.MustQuery("select /*+ INL_MERGE_JOIN(t, s) */ t.a from t left join s on t.a = s.a order by t.a desc").Check(testkit.Rows("148307968", "-277544960", "-277544960", "-1327693824"))

	tk.MustExec("DROP TABLE IF EXISTS t;")
	tk.MustExec("CREATE TABLE t(a BIGINT PRIMARY KEY, b BIGINT);")
	tk.MustExec("INSERT INTO t VALUES(1, 2);")
	tk.MustQuery("SELECT /*+ INL_JOIN(t1, t2) */ * FROM t t1 JOIN t t2 ON t1.a=t2.a UNION ALL SELECT /*+ INL_JOIN(t1, t2) */ * FROM t t1 JOIN t t2 ON t1.a=t2.a;").Check(testkit.Rows("1 2 1 2", "1 2 1 2"))
	tk.MustQuery("SELECT /*+ INL_HASH_JOIN(t1, t2) */ * FROM t t1 JOIN t t2 ON t1.a=t2.a UNION ALL SELECT /*+ INL_HASH_JOIN(t1, t2) */ * FROM t t1 JOIN t t2 ON t1.a=t2.a;").Check(testkit.Rows("1 2 1 2", "1 2 1 2"))
	tk.MustQuery("SELECT /*+ INL_MERGE_JOIN(t1, t2) */ * FROM t t1 JOIN t t2 ON t1.a=t2.a UNION ALL SELECT /*+ INL_MERGE_JOIN(t1, t2) */ * FROM t t1 JOIN t t2 ON t1.a=t2.a;").Check(testkit.Rows("1 2 1 2", "1 2 1 2"))

	tk.MustExec(`drop table if exists t;`)
	tk.MustExec(`create table t(a decimal(6,2), index idx(a));`)
	tk.MustExec(`insert into t values(1.01), (2.02), (NULL);`)
	tk.MustQuery(`select /*+ INL_JOIN(t2) */ t1.a from t t1 join t t2 on t1.a=t2.a order by t1.a;`).Check(testkit.Rows(
		`1.01`,
		`2.02`,
	))
	tk.MustQuery(`select /*+ INL_HASH_JOIN(t2) */ t1.a from t t1 join t t2 on t1.a=t2.a order by t1.a;`).Check(testkit.Rows(
		`1.01`,
		`2.02`,
	))
	tk.MustQuery(`select /*+ INL_MERGE_JOIN(t2) */ t1.a from t t1 join t t2 on t1.a=t2.a order by t1.a;`).Check(testkit.Rows(
		`1.01`,
		`2.02`,
	))

	tk.MustExec(`drop table if exists t;`)
	tk.MustExec(`create table t(a bigint, b bigint, unique key idx1(a, b));`)
	tk.MustExec(`insert into t values(1, 1), (1, 2), (1, 3), (1, 4), (1, 5), (1, 6);`)
	tk.MustExec(`set @@tidb_init_chunk_size = 2;`)
	tk.MustQuery(`select /*+ INL_JOIN(t2) */ * from t t1 left join t t2 on t1.a = t2.a and t1.b = t2.b + 4;`).Check(testkit.Rows(
		`1 1 <nil> <nil>`,
		`1 2 <nil> <nil>`,
		`1 3 <nil> <nil>`,
		`1 4 <nil> <nil>`,
		`1 5 1 1`,
		`1 6 1 2`,
	))
	tk.MustQuery(`select /*+ INL_HASH_JOIN(t2) */ * from t t1 left join t t2 on t1.a = t2.a and t1.b = t2.b + 4;`).Check(testkit.Rows(
		`1 1 <nil> <nil>`,
		`1 2 <nil> <nil>`,
		`1 3 <nil> <nil>`,
		`1 4 <nil> <nil>`,
		`1 5 1 1`,
		`1 6 1 2`,
	))
	tk.MustQuery(`select /*+ INL_MERGE_JOIN(t2) */ * from t t1 left join t t2 on t1.a = t2.a and t1.b = t2.b + 4;`).Check(testkit.Rows(
		`1 1 <nil> <nil>`,
		`1 2 <nil> <nil>`,
		`1 3 <nil> <nil>`,
		`1 4 <nil> <nil>`,
		`1 5 1 1`,
		`1 6 1 2`,
	))

	tk.MustExec(`drop table if exists t1, t2, t3;`)
	tk.MustExec("create table t1(a int primary key, b int)")
	tk.MustExec("insert into t1 values(1, 0), (2, null)")
	tk.MustExec("create table t2(a int primary key)")
	tk.MustExec("insert into t2 values(0)")
	tk.MustQuery("select /*+ INL_JOIN(t2)*/ * from t1 left join t2 on t1.b = t2.a;").Sort().Check(testkit.Rows(
		`1 0 0`,
		`2 <nil> <nil>`,
	))
	tk.MustQuery("select /*+ INL_HASH_JOIN(t2)*/ * from t1 left join t2 on t1.b = t2.a;").Sort().Check(testkit.Rows(
		`1 0 0`,
		`2 <nil> <nil>`,
	))
	tk.MustQuery("select /*+ INL_MERGE_JOIN(t2)*/ * from t1 left join t2 on t1.b = t2.a;").Sort().Check(testkit.Rows(
		`1 0 0`,
		`2 <nil> <nil>`,
	))

	tk.MustExec("create table t3(a int, key(a))")
	tk.MustExec("insert into t3 values(0)")
	tk.MustQuery("select /*+ INL_JOIN(t3)*/ * from t1 left join t3 on t1.b = t3.a;").Check(testkit.Rows(
		`1 0 0`,
		`2 <nil> <nil>`,
	))
	tk.MustQuery("select /*+ INL_HASH_JOIN(t3)*/ * from t1 left join t3 on t1.b = t3.a;").Check(testkit.Rows(
		`1 0 0`,
		`2 <nil> <nil>`,
	))
	tk.MustQuery("select /*+ INL_MERGE_JOIN(t3)*/ * from t1 left join t3 on t1.b = t3.a;").Check(testkit.Rows(
		`2 <nil> <nil>`,
		`1 0 0`,
	))

	tk.MustExec("drop table if exists t,s")
	tk.MustExec("create table t(a int primary key auto_increment, b time)")
	tk.MustExec("create table s(a int, b time)")
	tk.MustExec("alter table s add index idx(a,b)")
	tk.MustExec("set @@tidb_index_join_batch_size=4;set @@tidb_init_chunk_size=1;set @@tidb_max_chunk_size=32; set @@tidb_index_lookup_join_concurrency=15;")
	tk.MustExec("set @@session.tidb_executor_concurrency = 4;")
	tk.MustExec("set @@session.tidb_hash_join_concurrency = 5;")

	// insert 64 rows into `t`
	tk.MustExec("insert into t values(0, '01:01:01')")
	for i := 0; i < 6; i++ {
		tk.MustExec("insert into t select 0, b + 1 from t")
	}
	tk.MustExec("insert into s select a, b - 1 from t")
	tk.MustExec("analyze table t;")
	tk.MustExec("analyze table s;")

	tk.MustQuery("desc format = 'brief' select /*+ TIDB_INLJ(s) */ count(*) from t join s use index(idx) on s.a = t.a and s.b < t.b").Check(testkit.Rows(
		"HashAgg 1.00 root  funcs:count(1)->Column#6",
		"└─IndexJoin 64.00 root  inner join, inner:IndexReader, outer key:test.t.a, inner key:test.s.a, equal cond:eq(test.t.a, test.s.a), other cond:lt(test.s.b, test.t.b)",
		"  ├─TableReader(Build) 64.00 root  data:Selection",
		"  │ └─Selection 64.00 cop[tikv]  not(isnull(test.t.b))",
		"  │   └─TableFullScan 64.00 cop[tikv] table:t keep order:false",
		"  └─IndexReader(Probe) 64.00 root  index:Selection",
		"    └─Selection 64.00 cop[tikv]  not(isnull(test.s.a)), not(isnull(test.s.b))",
		"      └─IndexRangeScan 64.00 cop[tikv] table:s, index:idx(a, b) range: decided by [eq(test.s.a, test.t.a) lt(test.s.b, test.t.b)], keep order:false"))
	tk.MustQuery("select /*+ TIDB_INLJ(s) */ count(*) from t join s use index(idx) on s.a = t.a and s.b < t.b").Check(testkit.Rows("64"))
	tk.MustExec("set @@tidb_index_lookup_join_concurrency=1;")
	tk.MustQuery("select /*+ TIDB_INLJ(s) */ count(*) from t join s use index(idx) on s.a = t.a and s.b < t.b").Check(testkit.Rows("64"))

	tk.MustQuery("desc format = 'brief' select /*+ INL_MERGE_JOIN(s) */ count(*) from t join s use index(idx) on s.a = t.a and s.b < t.b").Check(testkit.Rows(
		"HashAgg 1.00 root  funcs:count(1)->Column#6",
		"└─IndexMergeJoin 64.00 root  inner join, inner:IndexReader, outer key:test.t.a, inner key:test.s.a, other cond:lt(test.s.b, test.t.b)",
		"  ├─TableReader(Build) 64.00 root  data:Selection",
		"  │ └─Selection 64.00 cop[tikv]  not(isnull(test.t.b))",
		"  │   └─TableFullScan 64.00 cop[tikv] table:t keep order:false",
		"  └─IndexReader(Probe) 64.00 root  index:Selection",
		"    └─Selection 64.00 cop[tikv]  not(isnull(test.s.a)), not(isnull(test.s.b))",
		"      └─IndexRangeScan 64.00 cop[tikv] table:s, index:idx(a, b) range: decided by [eq(test.s.a, test.t.a) lt(test.s.b, test.t.b)], keep order:true",
	))
	tk.MustQuery("select /*+ INL_MERGE_JOIN(s) */ count(*) from t join s use index(idx) on s.a = t.a and s.b < t.b").Check(testkit.Rows("64"))
	tk.MustExec("set @@tidb_index_lookup_join_concurrency=1;")
	tk.MustQuery("select /*+ INL_MERGE_JOIN(s) */ count(*) from t join s use index(idx) on s.a = t.a and s.b < t.b").Check(testkit.Rows("64"))

	tk.MustQuery("desc format = 'brief' select /*+ INL_HASH_JOIN(s) */ count(*) from t join s use index(idx) on s.a = t.a and s.b < t.b").Check(testkit.Rows(
		"HashAgg 1.00 root  funcs:count(1)->Column#6",
		"└─IndexHashJoin 64.00 root  inner join, inner:IndexReader, outer key:test.t.a, inner key:test.s.a, equal cond:eq(test.t.a, test.s.a), other cond:lt(test.s.b, test.t.b)",
		"  ├─TableReader(Build) 64.00 root  data:Selection",
		"  │ └─Selection 64.00 cop[tikv]  not(isnull(test.t.b))",
		"  │   └─TableFullScan 64.00 cop[tikv] table:t keep order:false",
		"  └─IndexReader(Probe) 64.00 root  index:Selection",
		"    └─Selection 64.00 cop[tikv]  not(isnull(test.s.a)), not(isnull(test.s.b))",
		"      └─IndexRangeScan 64.00 cop[tikv] table:s, index:idx(a, b) range: decided by [eq(test.s.a, test.t.a) lt(test.s.b, test.t.b)], keep order:false",
	))
	tk.MustQuery("select /*+ INL_HASH_JOIN(s) */ count(*) from t join s use index(idx) on s.a = t.a and s.b < t.b").Check(testkit.Rows("64"))
	tk.MustExec("set @@tidb_index_lookup_join_concurrency=1;")
	tk.MustQuery("select /*+ INL_HASH_JOIN(s) */ count(*) from t join s use index(idx) on s.a = t.a and s.b < t.b").Check(testkit.Rows("64"))

	// issue15658
	tk.MustExec("drop table t1, t2")
	tk.MustExec("create table t1(id int primary key)")
	tk.MustExec("create table t2(a int, b int)")
	tk.MustExec("insert into t1 values(1)")
	tk.MustExec("insert into t2 values(1,1),(2,1)")
	tk.MustQuery("select /*+ inl_join(t1)*/ * from t1 join t2 on t2.b=t1.id and t2.a=t1.id;").Check(testkit.Rows("1 1 1"))
	tk.MustQuery("select /*+ inl_hash_join(t1)*/ * from t1 join t2 on t2.b=t1.id and t2.a=t1.id;").Check(testkit.Rows("1 1 1"))
	tk.MustQuery("select /*+ inl_merge_join(t1)*/ * from t1 join t2 on t2.b=t1.id and t2.a=t1.id;").Check(testkit.Rows("1 1 1"))
}

func TestIndexNestedLoopHashJoin(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("set @@tidb_init_chunk_size=2")
	tk.MustExec("set @@tidb_index_join_batch_size=10")
	tk.MustExec("DROP TABLE IF EXISTS t, s")
	tk.Session().GetSessionVars().EnableClusteredIndex = variable.ClusteredIndexDefModeIntOnly
	tk.MustExec("create table t(pk int primary key, a int)")
	for i := 0; i < 100; i++ {
		tk.MustExec(fmt.Sprintf("insert into t values(%d, %d)", i, i))
	}
	tk.MustExec("create table s(a int primary key)")
	for i := 0; i < 100; i++ {
		if rand.Float32() < 0.3 {
			tk.MustExec(fmt.Sprintf("insert into s values(%d)", i))
		} else {
			tk.MustExec(fmt.Sprintf("insert into s values(%d)", i*100))
		}
	}
	tk.MustExec("analyze table t")
	tk.MustExec("analyze table s")
	// Test IndexNestedLoopHashJoin keepOrder.
	tk.MustQuery("explain format = 'brief' select /*+ INL_HASH_JOIN(s) */ * from t left join s on t.a=s.a order by t.pk").Check(testkit.Rows(
		"IndexHashJoin 100.00 root  left outer join, inner:TableReader, outer key:test.t.a, inner key:test.s.a, equal cond:eq(test.t.a, test.s.a)",
		"├─TableReader(Build) 100.00 root  data:TableFullScan",
		"│ └─TableFullScan 100.00 cop[tikv] table:t keep order:true",
		"└─TableReader(Probe) 100.00 root  data:TableRangeScan",
		"  └─TableRangeScan 100.00 cop[tikv] table:s range: decided by [test.t.a], keep order:false",
	))
	rs := tk.MustQuery("select /*+ INL_HASH_JOIN(s) */ * from t left join s on t.a=s.a order by t.pk")
	for i, row := range rs.Rows() {
		require.Equal(t, fmt.Sprintf("%d", i), row[0].(string))
	}

	// index hash join with semi join
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/planner/core/MockOnlyEnableIndexHashJoin", "return(true)"))
	defer func() {
		require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/planner/core/MockOnlyEnableIndexHashJoin"))
	}()
	tk.MustExec("drop table t")
	tk.MustExec("CREATE TABLE `t` (	`l_orderkey` int(11) NOT NULL,`l_linenumber` int(11) NOT NULL,`l_partkey` int(11) DEFAULT NULL,`l_suppkey` int(11) DEFAULT NULL,PRIMARY KEY (`l_orderkey`,`l_linenumber`))")
	tk.MustExec(`insert into t values(0,0,0,0);`)
	tk.MustExec(`insert into t values(0,1,0,1);`)
	tk.MustExec(`insert into t values(0,2,0,0);`)
	tk.MustExec(`insert into t values(1,0,1,0);`)
	tk.MustExec(`insert into t values(1,1,1,1);`)
	tk.MustExec(`insert into t values(1,2,1,0);`)
	tk.MustExec(`insert into t values(2,0,0,0);`)
	tk.MustExec(`insert into t values(2,1,0,1);`)
	tk.MustExec(`insert into t values(2,2,0,0);`)

	tk.MustExec("analyze table t")

	// test semi join
	tk.Session().GetSessionVars().InitChunkSize = 2
	tk.Session().GetSessionVars().MaxChunkSize = 2
	tk.MustExec("set @@tidb_index_join_batch_size=2")
	tk.MustQuery("desc format = 'brief' select * from t l1 where exists ( select * from t l2 where l2.l_orderkey = l1.l_orderkey and l2.l_suppkey <> l1.l_suppkey ) order by `l_orderkey`,`l_linenumber`;").Check(testkit.Rows(
		"Sort 7.20 root  test.t.l_orderkey, test.t.l_linenumber",
		"└─IndexHashJoin 7.20 root  semi join, inner:IndexLookUp, outer key:test.t.l_orderkey, inner key:test.t.l_orderkey, equal cond:eq(test.t.l_orderkey, test.t.l_orderkey), other cond:ne(test.t.l_suppkey, test.t.l_suppkey)",
		"  ├─TableReader(Build) 9.00 root  data:Selection",
		"  │ └─Selection 9.00 cop[tikv]  not(isnull(test.t.l_suppkey))",
		"  │   └─TableFullScan 9.00 cop[tikv] table:l1 keep order:false",
		"  └─IndexLookUp(Probe) 27.00 root  ",
		"    ├─IndexRangeScan(Build) 27.00 cop[tikv] table:l2, index:PRIMARY(l_orderkey, l_linenumber) range: decided by [eq(test.t.l_orderkey, test.t.l_orderkey)], keep order:false",
		"    └─Selection(Probe) 27.00 cop[tikv]  not(isnull(test.t.l_suppkey))",
		"      └─TableRowIDScan 27.00 cop[tikv] table:l2 keep order:false"))
	tk.MustQuery("select * from t l1 where exists ( select * from t l2 where l2.l_orderkey = l1.l_orderkey and l2.l_suppkey <> l1.l_suppkey )order by `l_orderkey`,`l_linenumber`;").Check(testkit.Rows("0 0 0 0", "0 1 0 1", "0 2 0 0", "1 0 1 0", "1 1 1 1", "1 2 1 0", "2 0 0 0", "2 1 0 1", "2 2 0 0"))
	tk.MustQuery("desc format = 'brief' select count(*) from t l1 where exists ( select * from t l2 where l2.l_orderkey = l1.l_orderkey and l2.l_suppkey <> l1.l_suppkey );").Check(testkit.Rows(
		"StreamAgg 1.00 root  funcs:count(1)->Column#11",
		"└─IndexHashJoin 7.20 root  semi join, inner:IndexLookUp, outer key:test.t.l_orderkey, inner key:test.t.l_orderkey, equal cond:eq(test.t.l_orderkey, test.t.l_orderkey), other cond:ne(test.t.l_suppkey, test.t.l_suppkey)",
		"  ├─TableReader(Build) 9.00 root  data:Selection",
		"  │ └─Selection 9.00 cop[tikv]  not(isnull(test.t.l_suppkey))",
		"  │   └─TableFullScan 9.00 cop[tikv] table:l1 keep order:false",
		"  └─IndexLookUp(Probe) 27.00 root  ",
		"    ├─IndexRangeScan(Build) 27.00 cop[tikv] table:l2, index:PRIMARY(l_orderkey, l_linenumber) range: decided by [eq(test.t.l_orderkey, test.t.l_orderkey)], keep order:false",
		"    └─Selection(Probe) 27.00 cop[tikv]  not(isnull(test.t.l_suppkey))",
		"      └─TableRowIDScan 27.00 cop[tikv] table:l2 keep order:false"))
	tk.MustQuery("select count(*) from t l1 where exists ( select * from t l2 where l2.l_orderkey = l1.l_orderkey and l2.l_suppkey <> l1.l_suppkey );").Check(testkit.Rows("9"))
	tk.MustExec("DROP TABLE IF EXISTS t, s")

	// issue16586
	tk.MustExec("use test;")
	tk.MustExec("drop table if exists lineitem;")
	tk.MustExec("drop table if exists orders;")
	tk.MustExec("drop table if exists supplier;")
	tk.MustExec("drop table if exists nation;")
	tk.MustExec("CREATE TABLE `lineitem` (`l_orderkey` int(11) NOT NULL,`l_linenumber` int(11) NOT NULL,`l_partkey` int(11) DEFAULT NULL,`l_suppkey` int(11) DEFAULT NULL,PRIMARY KEY (`l_orderkey`,`l_linenumber`)	);")
	tk.MustExec("CREATE TABLE `supplier` (	`S_SUPPKEY` bigint(20) NOT NULL,`S_NATIONKEY` bigint(20) NOT NULL,PRIMARY KEY (`S_SUPPKEY`));")
	tk.MustExec("CREATE TABLE `orders` (`O_ORDERKEY` bigint(20) NOT NULL,`O_ORDERSTATUS` char(1) NOT NULL,PRIMARY KEY (`O_ORDERKEY`));")
	tk.MustExec("CREATE TABLE `nation` (`N_NATIONKEY` bigint(20) NOT NULL,`N_NAME` char(25) NOT NULL,PRIMARY KEY (`N_NATIONKEY`))")
	tk.MustExec("insert into lineitem values(0,0,0,1)")
	tk.MustExec("insert into lineitem values(0,1,1,1)")
	tk.MustExec("insert into lineitem values(0,2,2,0)")
	tk.MustExec("insert into lineitem values(0,3,3,3)")
	tk.MustExec("insert into lineitem values(0,4,1,4)")
	tk.MustExec("insert into supplier values(0, 4)")
	tk.MustExec("insert into orders values(0, 'F')")
	tk.MustExec("insert into nation values(0, 'EGYPT')")
	tk.MustExec("insert into lineitem values(1,0,2,4)")
	tk.MustExec("insert into lineitem values(1,1,1,0)")
	tk.MustExec("insert into lineitem values(1,2,3,3)")
	tk.MustExec("insert into lineitem values(1,3,1,0)")
	tk.MustExec("insert into lineitem values(1,4,1,3)")
	tk.MustExec("insert into supplier values(1, 1)")
	tk.MustExec("insert into orders values(1, 'F')")
	tk.MustExec("insert into nation values(1, 'EGYPT')")
	tk.MustExec("insert into lineitem values(2,0,1,2)")
	tk.MustExec("insert into lineitem values(2,1,3,4)")
	tk.MustExec("insert into lineitem values(2,2,2,0)")
	tk.MustExec("insert into lineitem values(2,3,3,1)")
	tk.MustExec("insert into lineitem values(2,4,4,3)")
	tk.MustExec("insert into supplier values(2, 3)")
	tk.MustExec("insert into orders values(2, 'F')")
	tk.MustExec("insert into nation values(2, 'EGYPT')")
	tk.MustExec("insert into lineitem values(3,0,4,3)")
	tk.MustExec("insert into lineitem values(3,1,4,3)")
	tk.MustExec("insert into lineitem values(3,2,2,2)")
	tk.MustExec("insert into lineitem values(3,3,0,0)")
	tk.MustExec("insert into lineitem values(3,4,1,0)")
	tk.MustExec("insert into supplier values(3, 1)")
	tk.MustExec("insert into orders values(3, 'F')")
	tk.MustExec("insert into nation values(3, 'EGYPT')")
	tk.MustExec("insert into lineitem values(4,0,2,2)")
	tk.MustExec("insert into lineitem values(4,1,4,2)")
	tk.MustExec("insert into lineitem values(4,2,0,2)")
	tk.MustExec("insert into lineitem values(4,3,0,1)")
	tk.MustExec("insert into lineitem values(4,4,2,2)")
	tk.MustExec("insert into supplier values(4, 4)")
	tk.MustExec("insert into orders values(4, 'F')")
	tk.MustExec("insert into nation values(4, 'EGYPT')")
	tk.MustQuery("select count(*) from supplier, lineitem l1, orders, nation where s_suppkey = l1.l_suppkey and o_orderkey = l1.l_orderkey and o_orderstatus = 'F' and  exists ( select * from lineitem l2 where l2.l_orderkey = l1.l_orderkey and l2.l_suppkey < l1.l_suppkey ) and s_nationkey = n_nationkey and n_name = 'EGYPT' order by l1.l_orderkey, l1.l_linenumber;").Check(testkit.Rows("18"))
	tk.MustExec("drop table lineitem")
	tk.MustExec("drop table nation")
	tk.MustExec("drop table supplier")
	tk.MustExec("drop table orders")
}

func TestIssue15686(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t, k;")
	tk.MustExec("create table k (a int, pk int primary key, index(a));")
	tk.MustExec("create table t (a int, pk int primary key, index(a));")
	tk.MustExec("insert into k values(0,8),(0,23),(1,21),(1,33),(1,52),(2,17),(2,34),(2,39),(2,40),(2,66),(2,67),(3,9),(3,25),(3,41),(3,48),(4,4),(4,11),(4,15),(4,26),(4,27),(4,31),(4,35),(4,45),(4,47),(4,49);")
	tk.MustExec("insert into t values(3,4),(3,5),(3,27),(3,29),(3,57),(3,58),(3,79),(3,84),(3,92),(3,95);")
	tk.MustQuery("select /*+ inl_join(t) */ count(*) from k left join t on k.a = t.a and k.pk > t.pk;").Check(testkit.Rows("33"))
	tk.MustQuery("select /*+ inl_hash_join(t) */ count(*) from k left join t on k.a = t.a and k.pk > t.pk;").Check(testkit.Rows("33"))
	tk.MustQuery("select /*+ inl_merge_join(t) */ count(*) from k left join t on k.a = t.a and k.pk > t.pk;").Check(testkit.Rows("33"))
}

func TestIssue13449(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t, s;")
	tk.MustExec("create table t(a int, index(a));")
	tk.MustExec("create table s(a int, index(a));")
	for i := 1; i <= 128; i++ {
		tk.MustExec(fmt.Sprintf("insert into t values(%d)", i))
	}
	tk.MustExec("insert into s values(1), (128)")
	tk.MustExec("set @@tidb_max_chunk_size=32;")
	tk.MustExec("set @@tidb_index_lookup_join_concurrency=1;")
	tk.MustExec("set @@tidb_index_join_batch_size=32;")

	tk.MustQuery("desc format = 'brief' select /*+ INL_HASH_JOIN(s) */ * from t join s on t.a=s.a order by t.a;").Check(testkit.Rows(
		"IndexHashJoin 12487.50 root  inner join, inner:IndexReader, outer key:test.t.a, inner key:test.s.a, equal cond:eq(test.t.a, test.s.a)",
		"├─IndexReader(Build) 9990.00 root  index:IndexFullScan",
		"│ └─IndexFullScan 9990.00 cop[tikv] table:t, index:a(a) keep order:true, stats:pseudo",
		"└─IndexReader(Probe) 12487.50 root  index:Selection",
		"  └─Selection 12487.50 cop[tikv]  not(isnull(test.s.a))",
		"    └─IndexRangeScan 12500.00 cop[tikv] table:s, index:a(a) range: decided by [eq(test.s.a, test.t.a)], keep order:false, stats:pseudo"))
	tk.MustQuery("select /*+ INL_HASH_JOIN(s) */ * from t join s on t.a=s.a order by t.a;").Check(testkit.Rows("1 1", "128 128"))
}

func TestMergejoinOrder(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1, t2;")
	tk.MustExec("create table t1(a bigint primary key, b bigint);")
	tk.MustExec("create table t2(a bigint primary key, b bigint);")
	tk.MustExec("insert into t1 values(1, 100), (2, 100), (3, 100), (4, 100), (5, 100);")
	tk.MustExec("insert into t2 select a*100, b*100 from t1;")

	tk.MustQuery("explain format = 'brief' select /*+ TIDB_SMJ(t2) */ * from t1 left outer join t2 on t1.a=t2.a and t1.a!=3 order by t1.a;").Check(testkit.Rows(
		"MergeJoin 10000.00 root  left outer join, left key:test.t1.a, right key:test.t2.a, left cond:[ne(test.t1.a, 3)]",
		"├─TableReader(Build) 6666.67 root  data:TableRangeScan",
		"│ └─TableRangeScan 6666.67 cop[tikv] table:t2 range:[-inf,3), (3,+inf], keep order:true, stats:pseudo",
		"└─TableReader(Probe) 10000.00 root  data:TableFullScan",
		"  └─TableFullScan 10000.00 cop[tikv] table:t1 keep order:true, stats:pseudo",
	))

	tk.MustExec("set @@tidb_init_chunk_size=1")
	tk.MustQuery("select /*+ TIDB_SMJ(t2) */ * from t1 left outer join t2 on t1.a=t2.a and t1.a!=3 order by t1.a;").Check(testkit.Rows(
		"1 100 <nil> <nil>",
		"2 100 <nil> <nil>",
		"3 100 <nil> <nil>",
		"4 100 <nil> <nil>",
		"5 100 <nil> <nil>",
	))

	tk.MustExec(`drop table if exists t;`)
	tk.MustExec(`create table t(a bigint, b bigint, index idx_1(a,b));`)
	tk.MustExec(`insert into t values(1, 1), (1, 2), (2, 1), (2, 2);`)
	tk.MustQuery(`select /*+ TIDB_SMJ(t1, t2) */ * from t t1 join t t2 on t1.b = t2.b and t1.a=t2.a;`).Check(testkit.Rows(
		`1 1 1 1`,
		`1 2 1 2`,
		`2 1 2 1`,
		`2 2 2 2`,
	))

	tk.MustExec(`drop table if exists t;`)
	tk.MustExec(`create table t(a decimal(6,2), index idx(a));`)
	tk.MustExec(`insert into t values(1.01), (2.02), (NULL);`)
	tk.MustQuery(`select /*+ TIDB_SMJ(t1) */ t1.a from t t1 join t t2 on t1.a=t2.a order by t1.a;`).Check(testkit.Rows(
		`1.01`,
		`2.02`,
	))
}

func TestEmbeddedOuterJoin(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1, t2")
	tk.MustExec("create table t1(a int, b int)")
	tk.MustExec("create table t2(a int, b int)")
	tk.MustExec("insert into t1 values(1, 1)")
	tk.MustQuery("select * from (t1 left join t2 on t1.a = t2.a) left join (t2 t3 left join t2 t4 on t3.a = t4.a) on t2.b = 1").
		Check(testkit.Rows("1 1 <nil> <nil> <nil> <nil> <nil> <nil>"))
}

func TestHashJoin(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1, t2")
	tk.MustExec("create table t1(a int, b int);")
	tk.MustExec("create table t2(a int, b int);")
	tk.MustExec("insert into t1 values(1,1),(2,2),(3,3),(4,4),(5,5);")
	tk.MustQuery("select count(*) from t1").Check(testkit.Rows("5"))
	tk.MustQuery("select count(*) from t2").Check(testkit.Rows("0"))
	tk.MustExec("set @@tidb_init_chunk_size=1;")
	result := tk.MustQuery("explain analyze select /*+ TIDB_HJ(t1, t2) */ * from t1 where exists (select a from t2 where t1.a = t2.a);")
	//   0                       1        2 3         4        5                                                                    6                                           7         8
	// 0 HashJoin_9              7992.00  0 root               time:959.436µs, loops:1, Concurrency:5, probe collision:0, build:0s  semi join, equal:[eq(test.t1.a, test.t2.a)] 0 Bytes   0 Bytes
	// 1 ├─TableReader_15(Build) 9990.00  0 root               time:583.499µs, loops:1, rpc num: 1, rpc time:563.325µs, proc keys:0 data:Selection_14                           141 Bytes N/A
	// 2 │ └─Selection_14        9990.00  0 cop[tikv]          time:53.674µs, loops:1                                               not(isnull(test.t2.a))                      N/A       N/A
	// 3 │   └─TableFullScan_13  10000.00 0 cop[tikv] table:t2 time:52.14µs, loops:1                                                keep order:false, stats:pseudo              N/A       N/A
	// 4 └─TableReader_12(Probe) 9990.00  5 root               time:779.503µs, loops:1, rpc num: 1, rpc time:794.929µs, proc keys:0 data:Selection_11                           241 Bytes N/A
	// 5   └─Selection_11        9990.00  5 cop[tikv]          time:243.395µs, loops:6                                              not(isnull(test.t1.a))                      N/A       N/A
	// 6     └─TableFullScan_10  10000.00 5 cop[tikv] table:t1 time:206.273µs, loops:6                                              keep order:false, stats:pseudo              N/A       N/A
	row := result.Rows()
	require.Equal(t, 7, len(row))
	innerActRows := row[1][2].(string)
	require.Equal(t, "0", innerActRows)
	outerActRows := row[4][2].(string)
	// FIXME: revert this result to 1 after TableReaderExecutor can handle initChunkSize.
	require.Equal(t, "5", outerActRows)
}

func TestJoinDifferentDecimals(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("Use test")
	tk.MustExec("Drop table if exists t1")
	tk.MustExec("Create table t1 (v int)")
	tk.MustExec("Insert into t1 value (1)")
	tk.MustExec("Insert into t1 value (2)")
	tk.MustExec("Insert into t1 value (3)")
	tk.MustExec("Drop table if exists t2")
	tk.MustExec("Create table t2 (v decimal(12, 3))")
	tk.MustExec("Insert into t2 value (1)")
	tk.MustExec("Insert into t2 value (2.0)")
	tk.MustExec("Insert into t2 value (000003.000000)")
	rst := tk.MustQuery("Select * from t1, t2 where t1.v = t2.v order by t1.v")
	row := rst.Rows()
	require.Equal(t, 3, len(row))
	rst.Check(testkit.Rows("1 1.000", "2 2.000", "3 3.000"))
}

func TestNullEmptyAwareSemiJoin(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int, b int, c int, index idx_a(a), index idb_b(b), index idx_c(c))")
	tk.MustExec("insert into t values(null, 1, 0), (1, 2, 0)")
	tests := []struct {
		sql string
	}{
		{
			"a, b from t t1 where a not in (select b from t t2)",
		},
		{
			"a, b from t t1 where a not in (select b from t t2 where t1.b = t2.a)",
		},
		{
			"a, b from t t1 where a not in (select a from t t2)",
		},
		{
			"a, b from t t1 where a not in (select a from t t2 where t1.b = t2.b)",
		},
		{
			"a, b from t t1 where a != all (select b from t t2)",
		},
		{
			"a, b from t t1 where a != all (select b from t t2 where t1.b = t2.a)",
		},
		{
			"a, b from t t1 where a != all (select a from t t2)",
		},
		{
			"a, b from t t1 where a != all (select a from t t2 where t1.b = t2.b)",
		},
		{
			"a, b from t t1 where not exists (select * from t t2 where t1.a = t2.b)",
		},
		{
			"a, b from t t1 where not exists (select * from t t2 where t1.a = t2.a)",
		},
	}
	results := []struct {
		result [][]interface{}
	}{
		{
			testkit.Rows(),
		},
		{
			testkit.Rows("1 2"),
		},
		{
			testkit.Rows(),
		},
		{
			testkit.Rows(),
		},
		{
			testkit.Rows(),
		},
		{
			testkit.Rows("1 2"),
		},
		{
			testkit.Rows(),
		},
		{
			testkit.Rows(),
		},
		{
			testkit.Rows("<nil> 1"),
		},
		{
			testkit.Rows("<nil> 1"),
		},
	}
	hints := [5]string{
		"/*+ HASH_JOIN(t1, t2) */",
		"/*+ MERGE_JOIN(t1, t2) */",
		"/*+ INL_JOIN(t1, t2) */",
		"/*+ INL_HASH_JOIN(t1, t2) */",
		"/*+ INL_MERGE_JOIN(t1, t2) */",
	}
	for i, tt := range tests {
		for _, hint := range hints {
			sql := fmt.Sprintf("select %s %s", hint, tt.sql)
			result := tk.MustQuery(sql)
			result.Check(results[i].result)
		}
	}

	tk.MustExec("truncate table t")
	tk.MustExec("insert into t values(1, null, 0), (2, 1, 0)")
	results = []struct {
		result [][]interface{}
	}{
		{
			testkit.Rows(),
		},
		{
			testkit.Rows("1 <nil>"),
		},
		{
			testkit.Rows(),
		},
		{
			testkit.Rows("1 <nil>"),
		},
		{
			testkit.Rows(),
		},
		{
			testkit.Rows("1 <nil>"),
		},
		{
			testkit.Rows(),
		},
		{
			testkit.Rows("1 <nil>"),
		},
		{
			testkit.Rows("2 1"),
		},
		{
			testkit.Rows(),
		},
	}
	for i, tt := range tests {
		for _, hint := range hints {
			sql := fmt.Sprintf("select %s %s", hint, tt.sql)
			result := tk.MustQuery(sql)
			result.Check(results[i].result)
		}
	}

	tk.MustExec("truncate table t")
	tk.MustExec("insert into t values(1, null, 0), (2, 1, 0), (null, 2, 0)")
	results = []struct {
		result [][]interface{}
	}{
		{
			testkit.Rows(),
		},
		{
			testkit.Rows("1 <nil>"),
		},
		{
			testkit.Rows(),
		},
		{
			testkit.Rows("1 <nil>"),
		},
		{
			testkit.Rows(),
		},
		{
			testkit.Rows("1 <nil>"),
		},
		{
			testkit.Rows(),
		},
		{
			testkit.Rows("1 <nil>"),
		},
		{
			testkit.Rows("<nil> 2"),
		},
		{
			testkit.Rows("<nil> 2"),
		},
	}
	for i, tt := range tests {
		for _, hint := range hints {
			sql := fmt.Sprintf("select %s %s", hint, tt.sql)
			result := tk.MustQuery(sql)
			result.Check(results[i].result)
		}
	}

	tk.MustExec("truncate table t")
	tk.MustExec("insert into t values(1, null, 0), (2, null, 0)")
	tests = []struct {
		sql string
	}{
		{
			"a, b from t t1 where b not in (select a from t t2)",
		},
	}
	results = []struct {
		result [][]interface{}
	}{
		{
			testkit.Rows(),
		},
	}
	for i, tt := range tests {
		for _, hint := range hints {
			sql := fmt.Sprintf("select %s %s", hint, tt.sql)
			result := tk.MustQuery(sql)
			result.Check(results[i].result)
		}
	}

	tk.MustExec("truncate table t")
	tk.MustExec("insert into t values(null, 1, 1), (2, 2, 2), (3, null, 3), (4, 4, 3)")
	tests = []struct {
		sql string
	}{
		{
			"a, b, a not in (select b from t t2) from t t1 order by a",
		},
		{
			"a, c, a not in (select c from t t2) from t t1 order by a",
		},
		{
			"a, b, a in (select b from t t2) from t t1 order by a",
		},
		{
			"a, c, a in (select c from t t2) from t t1 order by a",
		},
	}
	results = []struct {
		result [][]interface{}
	}{
		{
			testkit.Rows(
				"<nil> 1 <nil>",
				"2 2 0",
				"3 <nil> <nil>",
				"4 4 0",
			),
		},
		{
			testkit.Rows(
				"<nil> 1 <nil>",
				"2 2 0",
				"3 3 0",
				"4 3 1",
			),
		},
		{
			testkit.Rows(
				"<nil> 1 <nil>",
				"2 2 1",
				"3 <nil> <nil>",
				"4 4 1",
			),
		},
		{
			testkit.Rows(
				"<nil> 1 <nil>",
				"2 2 1",
				"3 3 1",
				"4 3 0",
			),
		},
	}
	for i, tt := range tests {
		for _, hint := range hints {
			sql := fmt.Sprintf("select %s %s", hint, tt.sql)
			result := tk.MustQuery(sql)
			result.Check(results[i].result)
		}
	}

	tk.MustExec("drop table if exists s")
	tk.MustExec("create table s(a int, b int)")
	tk.MustExec("insert into s values(1, 2)")
	tk.MustExec("truncate table t")
	tk.MustExec("insert into t values(null, null, 0)")
	tests = []struct {
		sql string
	}{
		{
			"a in (select b from t t2 where t2.a = t1.b) from s t1",
		},
		{
			"a in (select b from s t2 where t2.a = t1.b) from t t1",
		},
	}
	results = []struct {
		result [][]interface{}
	}{
		{
			testkit.Rows("0"),
		},
		{
			testkit.Rows("0"),
		},
	}
	for i, tt := range tests {
		for _, hint := range hints {
			sql := fmt.Sprintf("select %s %s", hint, tt.sql)
			result := tk.MustQuery(sql)
			result.Check(results[i].result)
		}
	}

	tk.MustExec("truncate table s")
	tk.MustExec("insert into s values(2, 2)")
	tk.MustExec("truncate table t")
	tk.MustExec("insert into t values(null, 1, 0)")
	tests = []struct {
		sql string
	}{
		{
			"a in (select a from s t2 where t2.b = t1.b) from t t1",
		},
		{
			"a in (select a from s t2 where t2.b < t1.b) from t t1",
		},
	}
	results = []struct {
		result [][]interface{}
	}{
		{
			testkit.Rows("0"),
		},
		{
			testkit.Rows("0"),
		},
	}
	for i, tt := range tests {
		for _, hint := range hints {
			sql := fmt.Sprintf("select %s %s", hint, tt.sql)
			result := tk.MustQuery(sql)
			result.Check(results[i].result)
		}
	}

	tk.MustExec("truncate table s")
	tk.MustExec("insert into s values(null, 2)")
	tk.MustExec("truncate table t")
	tk.MustExec("insert into t values(1, 1, 0)")
	tests = []struct {
		sql string
	}{
		{
			"a in (select a from s t2 where t2.b = t1.b) from t t1",
		},
		{
			"b in (select a from s t2) from t t1",
		},
		{
			"* from t t1 where a not in (select a from s t2 where t2.b = t1.b)",
		},
		{
			"* from t t1 where a not in (select a from s t2)",
		},
		{
			"* from s t1 where a not in (select a from t t2)",
		},
	}
	results = []struct {
		result [][]interface{}
	}{
		{
			testkit.Rows("0"),
		},
		{
			testkit.Rows("<nil>"),
		},
		{
			testkit.Rows("1 1 0"),
		},
		{
			testkit.Rows(),
		},
		{
			testkit.Rows(),
		},
	}
	for i, tt := range tests {
		for _, hint := range hints {
			sql := fmt.Sprintf("select %s %s", hint, tt.sql)
			result := tk.MustQuery(sql)
			result.Check(results[i].result)
		}
	}

	tk.MustExec("drop table if exists t1, t2")
	tk.MustExec("create table t1(a int)")
	tk.MustExec("create table t2(a int)")
	tk.MustExec("insert into t1 values(1),(2)")
	tk.MustExec("insert into t2 values(1),(null)")
	tk.MustQuery("select * from t1 where a not in (select a from t2 where t1.a = t2.a)").Check(testkit.Rows(
		"2",
	))
	tk.MustQuery("select * from t1 where a != all (select a from t2 where t1.a = t2.a)").Check(testkit.Rows(
		"2",
	))
	tk.MustQuery("select * from t1 where a <> all (select a from t2 where t1.a = t2.a)").Check(testkit.Rows(
		"2",
	))
}

func TestScalarFuncNullSemiJoin(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int, b int)")
	tk.MustExec("insert into t values(null, 1), (1, 2)")
	tk.MustExec("drop table if exists s")
	tk.MustExec("create table s(a varchar(20), b varchar(20))")
	tk.MustExec("insert into s values(null, '1')")
	tk.MustQuery("select a in (select a from s) from t").Check(testkit.Rows("<nil>", "<nil>"))
	tk.MustExec("drop table s")
	tk.MustExec("create table s(a int, b int)")
	tk.MustExec("insert into s values(null, 1)")
	tk.MustQuery("select a in (select a+b from s) from t").Check(testkit.Rows("<nil>", "<nil>"))
}

func TestInjectProjOnTopN(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("drop table if exists t2")
	tk.MustExec("create table t1(a bigint, b bigint)")
	tk.MustExec("create table t2(a bigint, b bigint)")
	tk.MustExec("insert into t1 values(1, 1)")
	tk.MustQuery("select t1.a+t1.b as result from t1 left join t2 on 1 = 0 order by result limit 20;").Check(testkit.Rows(
		"2",
	))
}

func TestIssue11544(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table 11544t(a int)")
	tk.MustExec("create table 11544tt(a int, b varchar(10), index idx(a, b(3)))")
	tk.MustExec("insert into 11544t values(1)")
	tk.MustExec("insert into 11544tt values(1, 'aaaaaaa'), (1, 'aaaabbb'), (1, 'aaaacccc')")
	tk.MustQuery("select /*+ INL_JOIN(tt) */ * from 11544t t, 11544tt tt where t.a=tt.a and (tt.b = 'aaaaaaa' or tt.b = 'aaaabbb')").Check(testkit.Rows("1 1 aaaaaaa", "1 1 aaaabbb"))
	tk.MustQuery("select /*+ INL_HASH_JOIN(tt) */ * from 11544t t, 11544tt tt where t.a=tt.a and (tt.b = 'aaaaaaa' or tt.b = 'aaaabbb')").Check(testkit.Rows("1 1 aaaaaaa", "1 1 aaaabbb"))
	// INL_MERGE_JOIN is invalid
	tk.MustQuery("select /*+ INL_MERGE_JOIN(tt) */ * from 11544t t, 11544tt tt where t.a=tt.a and (tt.b = 'aaaaaaa' or tt.b = 'aaaabbb')").Sort().Check(testkit.Rows("1 1 aaaaaaa", "1 1 aaaabbb"))

	tk.MustQuery("select /*+ INL_JOIN(tt) */ * from 11544t t, 11544tt tt where t.a=tt.a and tt.b in ('aaaaaaa', 'aaaabbb', 'aaaacccc')").Check(testkit.Rows("1 1 aaaaaaa", "1 1 aaaabbb", "1 1 aaaacccc"))
	tk.MustQuery("select /*+ INL_HASH_JOIN(tt) */ * from 11544t t, 11544tt tt where t.a=tt.a and tt.b in ('aaaaaaa', 'aaaabbb', 'aaaacccc')").Check(testkit.Rows("1 1 aaaaaaa", "1 1 aaaabbb", "1 1 aaaacccc"))
	// INL_MERGE_JOIN is invalid
	tk.MustQuery("select /*+ INL_MERGE_JOIN(tt) */ * from 11544t t, 11544tt tt where t.a=tt.a and tt.b in ('aaaaaaa', 'aaaabbb', 'aaaacccc')").Sort().Check(testkit.Rows("1 1 aaaaaaa", "1 1 aaaabbb", "1 1 aaaacccc"))
}

func TestIssue11390(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table 11390t (k1 int unsigned, k2 int unsigned, key(k1, k2))")
	tk.MustExec("insert into 11390t values(1, 1)")
	tk.MustQuery("select /*+ INL_JOIN(t1, t2) */ * from 11390t t1, 11390t t2 where t1.k2 > 0 and t1.k2 = t2.k2 and t2.k1=1;").Check(testkit.Rows("1 1 1 1"))
	tk.MustQuery("select /*+ INL_HASH_JOIN(t1, t2) */ * from 11390t t1, 11390t t2 where t1.k2 > 0 and t1.k2 = t2.k2 and t2.k1=1;").Check(testkit.Rows("1 1 1 1"))
	tk.MustQuery("select /*+ INL_MERGE_JOIN(t1, t2) */ * from 11390t t1, 11390t t2 where t1.k2 > 0 and t1.k2 = t2.k2 and t2.k1=1;").Check(testkit.Rows("1 1 1 1"))
}

func TestOuterTableBuildHashTableIsuse13933(t *testing.T) {
	plannercore.ForceUseOuterBuild4Test.Store(true)
	defer func() { plannercore.ForceUseOuterBuild4Test.Store(false) }()
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t, s")
	tk.MustExec("create table t (a int,b int)")
	tk.MustExec("create table s (a int,b int)")
	tk.MustExec("insert into t values (11,11),(1,2)")
	tk.MustExec("insert into s values (1,2),(2,1),(11,11)")
	tk.MustQuery("select * from t left join s on s.a > t.a").Sort().Check(testkit.Rows("1 2 11 11", "1 2 2 1", "11 11 <nil> <nil>"))
	tk.MustQuery("explain format = 'brief' select * from t left join s on s.a > t.a").Check(testkit.Rows(
		"HashJoin 99900000.00 root  CARTESIAN left outer join, other cond:gt(test.s.a, test.t.a)",
		"├─TableReader(Build) 10000.00 root  data:TableFullScan",
		"│ └─TableFullScan 10000.00 cop[tikv] table:t keep order:false, stats:pseudo",
		"└─TableReader(Probe) 9990.00 root  data:Selection",
		"  └─Selection 9990.00 cop[tikv]  not(isnull(test.s.a))",
		"    └─TableFullScan 10000.00 cop[tikv] table:s keep order:false, stats:pseudo"))
	tk.MustExec("drop table if exists t, s")
	tk.MustExec("Create table s (a int, b int, key(b))")
	tk.MustExec("Create table t (a int, b int, key(b))")
	tk.MustExec("Insert into s values (1,2),(2,1),(11,11)")
	tk.MustExec("Insert into t values (11,2),(1,2),(5,2)")
	tk.MustQuery("select /*+ INL_HASH_JOIN(s)*/ * from t left join s on s.b=t.b and s.a < t.a;").Sort().Check(testkit.Rows("1 2 <nil> <nil>", "11 2 1 2", "5 2 1 2"))
	tk.MustQuery("explain format = 'brief' select /*+ INL_HASH_JOIN(s)*/ * from t left join s on s.b=t.b and s.a < t.a;").Check(testkit.Rows(
		"IndexHashJoin 12475.01 root  left outer join, inner:IndexLookUp, outer key:test.t.b, inner key:test.s.b, equal cond:eq(test.t.b, test.s.b), other cond:lt(test.s.a, test.t.a)",
		"├─TableReader(Build) 10000.00 root  data:TableFullScan",
		"│ └─TableFullScan 10000.00 cop[tikv] table:t keep order:false, stats:pseudo",
		"└─IndexLookUp(Probe) 12475.01 root  ",
		"  ├─Selection(Build) 12487.50 cop[tikv]  not(isnull(test.s.b))",
		"  │ └─IndexRangeScan 12500.00 cop[tikv] table:s, index:b(b) range: decided by [eq(test.s.b, test.t.b)], keep order:false, stats:pseudo",
		"  └─Selection(Probe) 12475.01 cop[tikv]  not(isnull(test.s.a))",
		"    └─TableRowIDScan 12487.50 cop[tikv] table:s keep order:false, stats:pseudo"))
}

func TestIssue13177(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1, t2")
	tk.MustExec("create table t1(a varchar(20), b int, c int)")
	tk.MustExec("create table t2(a varchar(20), b int, c int, primary key(a, b))")
	tk.MustExec("insert into t1 values(\"abcd\", 1, 1), (\"bacd\", 2, 2), (\"cbad\", 3, 3)")
	tk.MustExec("insert into t2 values(\"bcd\", 1, 1), (\"acd\", 2, 2), (\"bad\", 3, 3)")
	tk.MustQuery("select /*+ inl_join(t1, t2) */ * from t1 join t2 on substr(t1.a, 2, 4) = t2.a and t1.b = t2.b where t1.c between 1 and 5").Sort().Check(testkit.Rows(
		"abcd 1 1 bcd 1 1",
		"bacd 2 2 acd 2 2",
		"cbad 3 3 bad 3 3",
	))
	tk.MustQuery("select /*+ inl_hash_join(t1, t2) */ * from t1 join t2 on substr(t1.a, 2, 4) = t2.a and t1.b = t2.b where t1.c between 1 and 5").Sort().Check(testkit.Rows(
		"abcd 1 1 bcd 1 1",
		"bacd 2 2 acd 2 2",
		"cbad 3 3 bad 3 3",
	))
	tk.MustQuery("select /*+ inl_merge_join(t1, t2) */ * from t1 join t2 on substr(t1.a, 2, 4) = t2.a and t1.b = t2.b where t1.c between 1 and 5").Sort().Check(testkit.Rows(
		"abcd 1 1 bcd 1 1",
		"bacd 2 2 acd 2 2",
		"cbad 3 3 bad 3 3",
	))
	tk.MustQuery("select /*+ inl_join(t1, t2) */ t1.* from t1 join t2 on substr(t1.a, 2, 4) = t2.a and t1.b = t2.b where t1.c between 1 and 5").Sort().Check(testkit.Rows(
		"abcd 1 1",
		"bacd 2 2",
		"cbad 3 3",
	))
	tk.MustQuery("select /*+ inl_hash_join(t1, t2) */ t1.* from t1 join t2 on substr(t1.a, 2, 4) = t2.a and t1.b = t2.b where t1.c between 1 and 5").Sort().Check(testkit.Rows(
		"abcd 1 1",
		"bacd 2 2",
		"cbad 3 3",
	))
	tk.MustQuery("select /*+ inl_merge_join(t1, t2) */ t1.* from t1 join t2 on substr(t1.a, 2, 4) = t2.a and t1.b = t2.b where t1.c between 1 and 5").Sort().Check(testkit.Rows(
		"abcd 1 1",
		"bacd 2 2",
		"cbad 3 3",
	))
}

func TestIssue14514(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t (pk varchar(14) primary key, a varchar(12));")
	tk.MustQuery("select * from (select t1.pk or '/' as c from t as t1 left join t as t2 on t1.a = t2.pk) as t where t.c = 1;").Check(testkit.Rows())
}

func TestOuterMatchStatusIssue14742(t *testing.T) {
	plannercore.ForceUseOuterBuild4Test.Store(true)
	defer func() { plannercore.ForceUseOuterBuild4Test.Store(false) }()
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists testjoin;")
	tk.MustExec("create table testjoin(a int);")
	tk.Session().GetSessionVars().MaxChunkSize = 2

	tk.MustExec("insert into testjoin values (NULL);")
	tk.MustExec("insert into testjoin values (1);")
	tk.MustExec("insert into testjoin values (2), (2), (2);")
	tk.MustQuery("SELECT * FROM testjoin t1 RIGHT JOIN testjoin t2 ON t1.a > t2.a order by t1.a, t2.a;").Check(testkit.Rows(
		"<nil> <nil>",
		"<nil> 2",
		"<nil> 2",
		"<nil> 2",
		"2 1",
		"2 1",
		"2 1",
	))
}

func TestInlineProjection4HashJoinIssue15316(t *testing.T) {
	// Two necessary factors to reproduce this issue:
	// (1) taking HashLeftJoin, i.e., letting the probing tuple lay at the left side of joined tuples
	// (2) the projection only contains a part of columns from the build side, i.e., pruning the same probe side
	plannercore.ForcedHashLeftJoin4Test.Store(true)
	defer func() { plannercore.ForcedHashLeftJoin4Test.Store(false) }()
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists S, T")
	tk.MustExec("create table S (a int not null, b int, c int);")
	tk.MustExec("create table T (a int not null, b int, c int);")
	tk.MustExec("insert into S values (0,1,2),(0,1,null),(0,1,2);")
	tk.MustExec("insert into T values (0,10,2),(0,10,null),(1,10,2);")
	tk.MustQuery("select T.a,T.a,T.c from S join T on T.a = S.a where S.b<T.b order by T.a,T.c;").Check(testkit.Rows(
		"0 0 <nil>",
		"0 0 <nil>",
		"0 0 <nil>",
		"0 0 2",
		"0 0 2",
		"0 0 2",
	))
	// NOTE: the HashLeftJoin should be kept
	tk.MustQuery("explain format = 'brief' select T.a,T.a,T.c from S join T on T.a = S.a where S.b<T.b order by T.a,T.c;").Check(testkit.Rows(
		"Sort 12487.50 root  test.t.a, test.t.c",
		"└─Projection 12487.50 root  test.t.a, test.t.a, test.t.c",
		"  └─HashJoin 12487.50 root  inner join, equal:[eq(test.s.a, test.t.a)], other cond:lt(test.s.b, test.t.b)",
		"    ├─TableReader(Build) 9990.00 root  data:Selection",
		"    │ └─Selection 9990.00 cop[tikv]  not(isnull(test.t.b))",
		"    │   └─TableFullScan 10000.00 cop[tikv] table:T keep order:false, stats:pseudo",
		"    └─TableReader(Probe) 9990.00 root  data:Selection",
		"      └─Selection 9990.00 cop[tikv]  not(isnull(test.s.b))",
		"        └─TableFullScan 10000.00 cop[tikv] table:S keep order:false, stats:pseudo"))
}

func TestIssue18070(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	defer tk.MustExec("SET GLOBAL tidb_mem_oom_action = DEFAULT")
	tk.MustExec("SET GLOBAL tidb_mem_oom_action='CANCEL'")
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1, t2")
	tk.MustExec("create table t1(a int, index(a))")
	tk.MustExec("create table t2(a int, index(a))")
	tk.MustExec("insert into t1 values(1),(2)")
	tk.MustExec("insert into t2 values(1),(1),(2),(2)")
	tk.MustExec("set @@tidb_mem_quota_query=1000")
	err := tk.QueryToErr("select /*+ inl_hash_join(t1)*/ * from t1 join t2 on t1.a = t2.a;")
	require.True(t, strings.Contains(err.Error(), "Out Of Memory Quota!"))

	fpName := "github.com/pingcap/tidb/executor/mockIndexMergeJoinOOMPanic"
	require.NoError(t, failpoint.Enable(fpName, `panic("ERROR 1105 (HY000): Out Of Memory Quota![conn_id=1]")`))
	defer func() {
		require.NoError(t, failpoint.Disable(fpName))
	}()
	err = tk.QueryToErr("select /*+ inl_merge_join(t1)*/ * from t1 join t2 on t1.a = t2.a;")
	require.True(t, strings.Contains(err.Error(), "Out Of Memory Quota!"))
}

func TestIssue18564(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1, t2")
	tk.MustExec("create table t1(a int, b int, primary key(a), index idx(b,a));")
	tk.MustExec("create table t2(a int, b int, primary key(a), index idx(b,a));")
	tk.MustExec("insert into t1 values(1, 1)")
	tk.MustExec("insert into t2 values(1, 1)")
	tk.MustQuery("select /*+ INL_JOIN(t1) */ * from t1 FORCE INDEX (idx) join t2 on t1.b=t2.b and t1.a = t2.a").Check(testkit.Rows("1 1 1 1"))
}

// test hash join when enum column got invalid value
func TestInvalidEnumVal(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test;")
	tk.MustExec("set sql_mode = '';")
	tk.MustExec("drop table if exists t1;")
	tk.MustExec("create table t1(c1 enum('a', 'b'));")
	tk.MustExec("insert into t1 values('a');")
	tk.MustExec("insert into t1 values(0);")
	tk.MustExec("insert into t1 values(100);")
	rows := tk.MustQuery("select /*+ hash_join(t_alias1, t_alias2)*/ * from t1 t_alias1 inner join t1 t_alias2 on t_alias1.c1 = t_alias2.c1;")
	// use empty string if got invalid enum int val.
	rows.Check(testkit.Rows("a a", " ", " ", " ", " "))
}

func TestIssue18572_1(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t1(a int, b int, index idx(b));")
	tk.MustExec("insert into t1 values(1, 1);")
	tk.MustExec("insert into t1 select * from t1;")

	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/executor/testIndexHashJoinInnerWorkerErr", "return"))
	defer func() {
		require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/executor/testIndexHashJoinInnerWorkerErr"))
	}()

	rs, err := tk.Exec("select /*+ inl_hash_join(t1) */ * from t1 right join t1 t2 on t1.b=t2.b;")
	require.NoError(t, err)
	_, err = session.GetRows4Test(context.Background(), nil, rs)
	require.True(t, strings.Contains(err.Error(), "mockIndexHashJoinInnerWorkerErr"))
	require.NoError(t, rs.Close())
}

func TestIssue18572_2(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t1(a int, b int, index idx(b));")
	tk.MustExec("insert into t1 values(1, 1);")
	tk.MustExec("insert into t1 select * from t1;")

	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/executor/testIndexHashJoinOuterWorkerErr", "return"))
	defer func() {
		require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/executor/testIndexHashJoinOuterWorkerErr"))
	}()

	rs, err := tk.Exec("select /*+ inl_hash_join(t1) */ * from t1 right join t1 t2 on t1.b=t2.b;")
	require.NoError(t, err)
	_, err = session.GetRows4Test(context.Background(), nil, rs)
	require.True(t, strings.Contains(err.Error(), "mockIndexHashJoinOuterWorkerErr"))
	require.NoError(t, rs.Close())
}

func TestIssue18572_3(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t1(a int, b int, index idx(b));")
	tk.MustExec("insert into t1 values(1, 1);")
	tk.MustExec("insert into t1 select * from t1;")

	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/executor/testIndexHashJoinBuildErr", "return"))
	defer func() {
		require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/executor/testIndexHashJoinBuildErr"))
	}()

	rs, err := tk.Exec("select /*+ inl_hash_join(t1) */ * from t1 right join t1 t2 on t1.b=t2.b;")
	require.NoError(t, err)
	_, err = session.GetRows4Test(context.Background(), nil, rs)
	require.True(t, strings.Contains(err.Error(), "mockIndexHashJoinBuildErr"))
	require.NoError(t, rs.Close())
}

func TestApplyOuterAggEmptyInput(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1, t2")
	tk.MustExec("create table t1(a int)")
	tk.MustExec("create table t2(a int)")
	tk.MustExec("insert into t1 values(1)")
	tk.MustExec("insert into t2 values(1)")
	tk.MustQuery("select count(1), (select count(1) from t2 where t2.a > t1.a) as field from t1 where t1.a = 100").Check(testkit.Rows(
		"0 <nil>",
	))
	tk.MustQuery("select /*+ agg_to_cop() */ count(1), (select count(1) from t2 where t2.a > t1.a) as field from t1 where t1.a = 100").Check(testkit.Rows(
		"0 <nil>",
	))
	tk.MustQuery("select count(1), (select count(1) from t2 where t2.a > t1.a) as field from t1 where t1.a = 1").Check(testkit.Rows(
		"1 0",
	))
	tk.MustQuery("select /*+ agg_to_cop() */ count(1), (select count(1) from t2 where t2.a > t1.a) as field from t1 where t1.a = 1").Check(testkit.Rows(
		"1 0",
	))
}

func TestIssue19112(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1, t2")
	tk.MustExec("create table t1 ( c_int int, c_decimal decimal(12, 6), key(c_int), unique key(c_decimal) )")
	tk.MustExec("create table t2 like t1")
	tk.MustExec("insert into t1 (c_int, c_decimal) values (1, 4.064000), (2, 0.257000), (3, 1.010000)")
	tk.MustExec("insert into t2 (c_int, c_decimal) values (1, 4.064000), (3, 1.010000)")
	tk.MustQuery("select /*+ HASH_JOIN(t1,t2) */  * from t1 join t2 on t1.c_decimal = t2.c_decimal order by t1.c_int").Check(testkit.Rows(
		"1 4.064000 1 4.064000",
		"3 1.010000 3 1.010000"))
}

func TestIssue11896(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")

	// compare bigint to bit(64)
	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t(c1 bigint)")
	tk.MustExec("create table t1(c1 bit(64))")
	tk.MustExec("insert into t value(1)")
	tk.MustExec("insert into t1 value(1)")
	tk.MustQuery("select * from t, t1 where t.c1 = t1.c1").Check(
		testkit.Rows("1 \x00\x00\x00\x00\x00\x00\x00\x01"))

	// compare int to bit(32)
	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t(c1 int)")
	tk.MustExec("create table t1(c1 bit(32))")
	tk.MustExec("insert into t value(1)")
	tk.MustExec("insert into t1 value(1)")
	tk.MustQuery("select * from t, t1 where t.c1 = t1.c1").Check(
		testkit.Rows("1 \x00\x00\x00\x01"))

	// compare mediumint to bit(24)
	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t(c1 mediumint)")
	tk.MustExec("create table t1(c1 bit(24))")
	tk.MustExec("insert into t value(1)")
	tk.MustExec("insert into t1 value(1)")
	tk.MustQuery("select * from t, t1 where t.c1 = t1.c1").Check(
		testkit.Rows("1 \x00\x00\x01"))

	// compare smallint to bit(16)
	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t(c1 smallint)")
	tk.MustExec("create table t1(c1 bit(16))")
	tk.MustExec("insert into t value(1)")
	tk.MustExec("insert into t1 value(1)")
	tk.MustQuery("select * from t, t1 where t.c1 = t1.c1").Check(
		testkit.Rows("1 \x00\x01"))

	// compare tinyint to bit(8)
	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t(c1 tinyint)")
	tk.MustExec("create table t1(c1 bit(8))")
	tk.MustExec("insert into t value(1)")
	tk.MustExec("insert into t1 value(1)")
	tk.MustQuery("select * from t, t1 where t.c1 = t1.c1").Check(
		testkit.Rows("1 \x01"))
}

func TestIssue19498(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")

	tk.MustExec("drop table if exists t1;")
	tk.MustExec("create table t1 (c_int int, primary key (c_int));")
	tk.MustExec("insert into t1 values (1),(2),(3),(4)")
	tk.MustExec("drop table if exists t2;")
	tk.MustExec("create table t2 (c_str varchar(40));")
	tk.MustExec("insert into t2 values ('zen sammet');")
	tk.MustExec("insert into t2 values ('happy fermat');")
	tk.MustExec("insert into t2 values ('happy archimedes');")
	tk.MustExec("insert into t2 values ('happy hypatia');")

	tk.MustExec("drop table if exists t3;")
	tk.MustExec("create table t3 (c_int int, c_str varchar(40), primary key (c_int), key (c_str));")
	tk.MustExec("insert into t3 values (1, 'sweet hoover');")
	tk.MustExec("insert into t3 values (2, 'awesome elion');")
	tk.MustExec("insert into t3 values (3, 'hungry khayyam');")
	tk.MustExec("insert into t3 values (4, 'objective kapitsa');")

	rs := tk.MustQuery("select c_str, (select /*+ INL_JOIN(t1,t3) */ max(t1.c_int) from t1, t3 where t1.c_int = t3.c_int and t2.c_str > t3.c_str) q from t2 order by c_str;")
	rs.Check(testkit.Rows("happy archimedes 2", "happy fermat 2", "happy hypatia 2", "zen sammet 4"))

	rs = tk.MustQuery("select c_str, (select /*+ INL_HASH_JOIN(t1,t3) */ max(t1.c_int) from t1, t3 where t1.c_int = t3.c_int and t2.c_str > t3.c_str) q from t2 order by c_str;")
	rs.Check(testkit.Rows("happy archimedes 2", "happy fermat 2", "happy hypatia 2", "zen sammet 4"))

	rs = tk.MustQuery("select c_str, (select /*+ INL_MERGE_JOIN(t1,t3) */ max(t1.c_int) from t1, t3 where t1.c_int = t3.c_int and t2.c_str > t3.c_str) q from t2 order by c_str;")
	rs.Check(testkit.Rows("happy archimedes 2", "happy fermat 2", "happy hypatia 2", "zen sammet 4"))
}

func TestIssue19500(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1;")
	tk.MustExec("create table t1 (c_int int, primary key (c_int));")
	tk.MustExec("insert into t1 values (1),(2),(3),(4),(5);")
	tk.MustExec("drop table if exists t2;")
	tk.MustExec("create table t2 (c_int int unsigned, c_str varchar(40), primary key (c_int), key (c_str));")
	tk.MustExec("insert into t2 values (1, 'dazzling panini'),(2, 'infallible perlman'),(3, 'recursing cannon'),(4, 'vigorous satoshi'),(5, 'vigilant gauss'),(6, 'nervous jackson');\n")
	tk.MustExec("drop table if exists t3;")
	tk.MustExec("create table t3 (c_int int, c_str varchar(40), key (c_str));")
	tk.MustExec("insert into t3 values (1, 'sweet morse'),(2, 'reverent golick'),(3, 'clever rubin'),(4, 'flamboyant morse');")
	tk.MustQuery("select (select (select sum(c_int) from t3 where t3.c_str > t2.c_str) from t2 where t2.c_int > t1.c_int order by c_int limit 1) q from t1 order by q;").
		Check(testkit.Rows("<nil>", "<nil>", "3", "3", "3"))
}

func TestExplainAnalyzeJoin(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1,t2;")
	tk.MustExec("create table t1 (a int, b int, unique index (a));")
	tk.MustExec("create table t2 (a int, b int, unique index (a))")
	tk.MustExec("insert into t1 values (1,1),(2,2),(3,3),(4,4),(5,5)")
	tk.MustExec("insert into t2 values (1,1),(2,2),(3,3),(4,4),(5,5)")
	// Test for index lookup join.
	rows := tk.MustQuery("explain analyze select /*+ INL_JOIN(t1, t2) */ * from t1,t2 where t1.a=t2.a;").Rows()
	require.Equal(t, 8, len(rows))
	require.Regexp(t, "IndexJoin_.*", rows[0][0])
	require.Regexp(t, "time:.*, loops:.*, inner:{total:.*, concurrency:.*, task:.*, construct:.*, fetch:.*, build:.*}, probe:.*", rows[0][5])
	// Test for index lookup hash join.
	rows = tk.MustQuery("explain analyze select /*+ INL_HASH_JOIN(t1, t2) */ * from t1,t2 where t1.a=t2.a;").Rows()
	require.Equal(t, 8, len(rows))
	require.Regexp(t, "IndexHashJoin.*", rows[0][0])
	require.Regexp(t, "time:.*, loops:.*, inner:{total:.*, concurrency:.*, task:.*, construct:.*, fetch:.*, build:.*, join:.*}", rows[0][5])
	// Test for hash join.
	rows = tk.MustQuery("explain analyze select /*+ HASH_JOIN(t1, t2) */ * from t1,t2 where t1.a=t2.a;").Rows()
	require.Equal(t, 7, len(rows))
	require.Regexp(t, "HashJoin.*", rows[0][0])
	require.Regexp(t, "time:.*, loops:.*, build_hash_table:{total:.*, fetch:.*, build:.*}, probe:{concurrency:5, total:.*, max:.*, probe:.*, fetch:.*}", rows[0][5])
	// Test for index merge join.
	rows = tk.MustQuery("explain analyze select /*+ INL_MERGE_JOIN(t1, t2) */ * from t1,t2 where t1.a=t2.a;").Rows()
	require.Len(t, rows, 9)
	require.Regexp(t, "IndexMergeJoin_.*", rows[0][0])
	require.Regexp(t, fmt.Sprintf(".*Concurrency:%v.*", tk.Session().GetSessionVars().IndexLookupJoinConcurrency()), rows[0][5])
}

func TestIssue20270(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t;")
	tk.MustExec("drop table if exists t1;")
	tk.MustExec("create table t(c1 int, c2 int)")
	tk.MustExec("create table t1(c1 int, c2 int)")
	tk.MustExec("insert into t values(1,1),(2,2)")
	tk.MustExec("insert into t1 values(2,3),(4,4)")
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/executor/killedInJoin2Chunk", "return(true)"))
	err := tk.QueryToErr("select /*+ TIDB_HJ(t, t1) */ * from t left join t1 on t.c1 = t1.c1 where t.c1 = 1 or t1.c2 > 20")
	require.Equal(t, executor.ErrQueryInterrupted, err)
	require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/executor/killedInJoin2Chunk"))
	plannercore.ForceUseOuterBuild4Test.Store(true)
	defer func() {
		plannercore.ForceUseOuterBuild4Test.Store(false)
	}()
	err = failpoint.Enable("github.com/pingcap/tidb/executor/killedInJoin2ChunkForOuterHashJoin", "return(true)")
	require.NoError(t, err)
	tk.MustExec("insert into t1 values(1,30),(2,40)")
	err = tk.QueryToErr("select /*+ TIDB_HJ(t, t1) */ * from t left outer join t1 on t.c1 = t1.c1 where t.c1 = 1 or t1.c2 > 20")
	require.Equal(t, executor.ErrQueryInterrupted, err)
	err = failpoint.Disable("github.com/pingcap/tidb/executor/killedInJoin2ChunkForOuterHashJoin")
	require.NoError(t, err)
}

func TestIssue20710(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t;")
	tk.MustExec("drop table if exists s;")
	tk.MustExec("create table t(a int, b int)")
	tk.MustExec("create table s(a int, b int, index(a))")
	tk.MustExec("insert into t values(1,1),(1,2),(2,2)")
	tk.MustExec("insert into s values(1,1),(2,2),(2,1)")
	tk.MustQuery("select /*+ inl_join(s) */ * from t join s on t.a=s.a and t.b = s.b").Sort().Check(testkit.Rows("1 1 1 1", "2 2 2 2"))
	tk.MustQuery("show warnings").Check(testkit.Rows())
	tk.MustQuery("select /*+ inl_join(s) */ * from t join s on t.a=s.a and t.b = s.a").Sort().Check(testkit.Rows("1 1 1 1", "2 2 2 1", "2 2 2 2"))
	tk.MustQuery("show warnings").Check(testkit.Rows())
	tk.MustQuery("select /*+ inl_join(s) */ * from t join s on t.a=s.a and t.a = s.b").Sort().Check(testkit.Rows("1 1 1 1", "1 2 1 1", "2 2 2 2"))
	tk.MustQuery("show warnings").Check(testkit.Rows())

	tk.MustQuery("select /*+ inl_join(s) */ * from t join s on t.a=s.a and t.b = s.b").Sort().Check(testkit.Rows("1 1 1 1", "2 2 2 2"))
	tk.MustQuery("show warnings").Check(testkit.Rows())
	tk.MustQuery("select /*+ inl_join(s) */ * from t join s on t.a=s.a and t.b = s.a").Sort().Check(testkit.Rows("1 1 1 1", "2 2 2 1", "2 2 2 2"))
	tk.MustQuery("show warnings").Check(testkit.Rows())
	tk.MustQuery("select /*+ inl_join(s) */ * from t join s on t.a=s.a and t.a = s.b").Sort().Check(testkit.Rows("1 1 1 1", "1 2 1 1", "2 2 2 2"))
	tk.MustQuery("show warnings").Check(testkit.Rows())
}

func TestIssue20779(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t1(a int, b int, index idx(b));")
	tk.MustExec("insert into t1 values(1, 1);")
	tk.MustExec("insert into t1 select * from t1;")

	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/executor/testIssue20779", "return"))
	defer func() {
		require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/executor/testIssue20779"))
	}()

	rs, err := tk.Exec("select /*+ inl_hash_join(t2) */ t1.b from t1 left join t1 t2 on t1.b=t2.b order by t1.b;")
	require.NoError(t, err)
	_, err = session.GetRows4Test(context.Background(), nil, rs)
	require.EqualError(t, err, "testIssue20779")
	require.NoError(t, rs.Close())
}

func TestIssue20219(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t,s ")
	tk.MustExec("CREATE TABLE `t` (   `a` set('a','b','c','d','e','f','g','h','i','j') DEFAULT NULL );")
	tk.MustExec("insert into t values('i'), ('j');")
	tk.MustExec("CREATE TABLE `s` (   `a` char(1) DEFAULT NULL,   KEY `a` (`a`) )")
	tk.MustExec("insert into s values('i'), ('j');")
	tk.MustQuery("select /*+ inl_hash_join(s)*/ t.a from t left join s on t.a = s.a;").Check(testkit.Rows("i", "j"))
	tk.MustQuery("show warnings").Check(testkit.Rows())
	tk.MustQuery("select /*+ inl_join(s)*/ t.a from t left join s on t.a = s.a;").Check(testkit.Rows("i", "j"))
	tk.MustQuery("show warnings").Check(testkit.Rows())
}

func TestIssue25902(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists tt1,tt2,tt3; ")
	tk.MustExec("create table tt1 (ts timestamp);")
	tk.MustExec("create table tt2 (ts varchar(32));")
	tk.MustExec("create table tt3 (ts datetime);")
	tk.MustExec("insert into tt1 values (\"2001-01-01 00:00:00\");")
	tk.MustExec("insert into tt2 values (\"2001-01-01 00:00:00\");")
	tk.MustExec("insert into tt3 values (\"2001-01-01 00:00:00\");")
	tk.MustQuery("select * from tt1 where ts in (select ts from tt2);").Check(testkit.Rows("2001-01-01 00:00:00"))
	tk.MustQuery("select * from tt1 where ts in (select ts from tt3);").Check(testkit.Rows("2001-01-01 00:00:00"))
	tk.MustExec("set @tmp=(select @@session.time_zone);")
	tk.MustExec("set @@session.time_zone = '+10:00';")
	tk.MustQuery("select * from tt1 where ts in (select ts from tt2);").Check(testkit.Rows())
	tk.MustExec("set @@session.time_zone = @tmp;")
}

func TestIssue30211(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1, t2;")
	tk.MustExec("create table t1(a int, index(a));")
	tk.MustExec("create table t2(a int, index(a));")
	func() {
		fpName := "github.com/pingcap/tidb/executor/TestIssue30211"
		require.NoError(t, failpoint.Enable(fpName, `panic("TestIssue30211 IndexJoinPanic")`))
		defer func() {
			require.NoError(t, failpoint.Disable(fpName))
		}()
		err := tk.QueryToErr("select /*+ inl_join(t1) */ * from t1 join t2 on t1.a = t2.a;")
		require.EqualError(t, err, "failpoint panic: TestIssue30211 IndexJoinPanic")

		err = tk.QueryToErr("select /*+ inl_hash_join(t1) */ * from t1 join t2 on t1.a = t2.a;")
		require.EqualError(t, err, "failpoint panic: TestIssue30211 IndexJoinPanic")
	}()
	tk.MustExec("insert into t1 values(1),(2);")
	tk.MustExec("insert into t2 values(1),(1),(2),(2);")
	tk.MustExec("set @@tidb_mem_quota_query=8000;")
	tk.MustExec("set tidb_index_join_batch_size = 1;")
	tk.MustExec("SET GLOBAL tidb_mem_oom_action = 'CANCEL'")
	defer tk.MustExec("SET GLOBAL tidb_mem_oom_action='LOG'")
	err := tk.QueryToErr("select /*+ inl_join(t1) */ * from t1 join t2 on t1.a = t2.a;").Error()
	require.True(t, strings.Contains(err, "Out Of Memory Quota"))
	err = tk.QueryToErr("select /*+ inl_hash_join(t1) */ * from t1 join t2 on t1.a = t2.a;").Error()
	require.True(t, strings.Contains(err, "Out Of Memory Quota"))
}

func TestIssue31129(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("set @@tidb_init_chunk_size=2")
	tk.MustExec("set @@tidb_index_join_batch_size=10")
	tk.MustExec("DROP TABLE IF EXISTS t, s")
	tk.Session().GetSessionVars().EnableClusteredIndex = variable.ClusteredIndexDefModeIntOnly
	tk.MustExec("create table t(pk int primary key, a int)")
	for i := 0; i < 100; i++ {
		tk.MustExec(fmt.Sprintf("insert into t values(%d, %d)", i, i))
	}
	tk.MustExec("create table s(a int primary key)")
	for i := 0; i < 100; i++ {
		tk.MustExec(fmt.Sprintf("insert into s values(%d)", i))
	}
	tk.MustExec("analyze table t")
	tk.MustExec("analyze table s")

	// Test IndexNestedLoopHashJoin keepOrder.
	fpName := "github.com/pingcap/tidb/executor/TestIssue31129"
	require.NoError(t, failpoint.Enable(fpName, "return"))
	err := tk.QueryToErr("select /*+ INL_HASH_JOIN(s) */ * from t left join s on t.a=s.a order by t.pk")
	require.True(t, strings.Contains(err.Error(), "TestIssue31129"))
	require.NoError(t, failpoint.Disable(fpName))

	// Test IndexNestedLoopHashJoin build hash table panic.
	fpName = "github.com/pingcap/tidb/executor/IndexHashJoinBuildHashTablePanic"
	require.NoError(t, failpoint.Enable(fpName, `panic("IndexHashJoinBuildHashTablePanic")`))
	err = tk.QueryToErr("select /*+ INL_HASH_JOIN(s) */ * from t left join s on t.a=s.a order by t.pk")
	require.True(t, strings.Contains(err.Error(), "IndexHashJoinBuildHashTablePanic"))
	require.NoError(t, failpoint.Disable(fpName))

	// Test IndexNestedLoopHashJoin fetch inner fail.
	fpName = "github.com/pingcap/tidb/executor/IndexHashJoinFetchInnerResultsErr"
	require.NoError(t, failpoint.Enable(fpName, "return"))
	err = tk.QueryToErr("select /*+ INL_HASH_JOIN(s) */ * from t left join s on t.a=s.a order by t.pk")
	require.True(t, strings.Contains(err.Error(), "IndexHashJoinFetchInnerResultsErr"))
	require.NoError(t, failpoint.Disable(fpName))

	// Test IndexNestedLoopHashJoin build hash table panic and IndexNestedLoopHashJoin fetch inner fail at the same time.
	fpName1, fpName2 := "github.com/pingcap/tidb/executor/IndexHashJoinBuildHashTablePanic", "github.com/pingcap/tidb/executor/IndexHashJoinFetchInnerResultsErr"
	require.NoError(t, failpoint.Enable(fpName1, `panic("IndexHashJoinBuildHashTablePanic")`))
	require.NoError(t, failpoint.Enable(fpName2, "return"))
	err = tk.QueryToErr("select /*+ INL_HASH_JOIN(s) */ * from t left join s on t.a=s.a order by t.pk")
	require.True(t, strings.Contains(err.Error(), "IndexHashJoinBuildHashTablePanic"))
	require.NoError(t, failpoint.Disable(fpName1))
	require.NoError(t, failpoint.Disable(fpName2))
}

func TestIssue37932(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk1 := testkit.NewTestKit(t, store)
	tk2 := testkit.NewTestKit(t, store)
	tk1.MustExec("use test")
	tk2.MustExec("use test")
	tk1.MustExec("create table tbl_1 ( col_1 set ( 'Alice','Bob','Charlie','David' )   not null default 'Alice' ,col_2 tinyint  unsigned ,col_3 decimal ( 34 , 3 )   not null default 79 ,col_4 bigint  unsigned not null ,col_5 bit ( 12 )   not null , unique key idx_1 ( col_2 ) ,unique key idx_2 ( col_2 ) ) charset utf8mb4 collate utf8mb4_bin ;")
	tk1.MustExec("create table tbl_2 ( col_6 text ( 52 ) collate utf8_unicode_ci  not null ,col_7 int  unsigned not null ,col_8 blob ( 369 ) ,col_9 bit ( 51 ) ,col_10 decimal ( 38 , 16 ) , unique key idx_3 ( col_7 ) ,unique key idx_4 ( col_7 ) ) charset utf8 collate utf8_unicode_ci ;")
	tk1.MustExec("create table tbl_3 ( col_11 set ( 'Alice','Bob','Charlie','David' )   not null ,col_12 bigint  unsigned not null default 1678891638492596595 ,col_13 text ( 18 ) ,col_14 set ( 'Alice','Bob','Charlie','David' )   not null default 'Alice' ,col_15 mediumint , key idx_5 ( col_12 ) ,unique key idx_6 ( col_12 ) ) charset utf8mb4 collate utf8mb4_general_ci ;")
	tk1.MustExec("create table tbl_4 ( col_16 set ( 'Alice','Bob','Charlie','David' )   not null ,col_17 tinyint  unsigned ,col_18 int  unsigned not null default 4279145838 ,col_19 varbinary ( 210 )   not null ,col_20 timestamp , primary key  ( col_18 ) /*T![clustered_index] nonclustered */ ,key idx_8 ( col_19 ) ) charset utf8mb4 collate utf8mb4_unicode_ci ;")
	tk1.MustExec("create table tbl_5 ( col_21 bigint ,col_22 set ( 'Alice','Bob','Charlie','David' ) ,col_23 blob ( 311 ) ,col_24 bigint  unsigned not null default 3415443099312152509 ,col_25 time , unique key idx_9 ( col_21 ) ,unique key idx_10 ( col_21 ) ) charset gbk collate gbk_bin ;")
	tk1.MustExec("insert into tbl_1 values ( 'Bob',null,0.04,2650749963804575036,4044 );")
	tk1.MustExec("insert into tbl_1 values ( 'Alice',171,1838.2,6452757231340518222,1190 );")
	tk1.MustExec("insert into tbl_1 values ( 'Bob',202,2.962,4304284252076747481,2112 );")
	tk1.MustExec("insert into tbl_1 values ( 'David',155,32610.05,5899651588546531414,104 );")
	tk1.MustExec("insert into tbl_1 values ( 'Charlie',52,4219.7,6151233689319516187,1246 );")
	tk1.MustExec("insert into tbl_1 values ( 'Bob',55,3963.11,3614977408465893392,1188 );")
	tk1.MustExec("insert into tbl_1 values ( 'Alice',203,72.01,1553550133494908281,1658 );")
	tk1.MustExec("insert into tbl_1 values ( 'Bob',40,871.569,8114062926218465773,1397 );")
	tk1.MustExec("insert into tbl_1 values ( 'Alice',165,7765,4481202107781982005,2089 );")
	tk1.MustExec("insert into tbl_1 values ( 'David',79,7.02,993594504887208796,514 );")
	tk1.MustExec("insert into tbl_2 values ( 'iB_%7c&q!6-gY4bkvg',2064909882,'dLN52t1YZSdJ',2251679806445488,32 );")
	tk1.MustExec("insert into tbl_2 values ( 'h_',1478443689,'EqP+iN=',180492371752598,0.1 );")
	tk1.MustExec("insert into tbl_2 values ( 'U@U&*WKfPzil=6YaDxp',4271201457,'QWuo24qkSSo',823931105457505,88514 );")
	tk1.MustExec("insert into tbl_2 values ( 'FR4GA=',505128825,'RpEmV6ph5Z7',568030123046798,609381 );")
	tk1.MustExec("insert into tbl_2 values ( '3GsU',166660047,'',1061132816887762,6.4605 );")
	tk1.MustExec("insert into tbl_2 values ( 'BA4hPRD0lm*pbg#NE',3440634757,'7gUPe2',288001159469205,6664.9 );")
	tk1.MustExec("insert into tbl_2 values ( '+z',2117152318,'WTkD(N',215697667226264,7.88 );")
	tk1.MustExec("insert into tbl_2 values ( 'x@SPhy9lOomPa4LF',2881759652,'ETUXQQ0b4HnBSKgTWIU',153379720424625,null );")
	tk1.MustExec("insert into tbl_2 values ( '',2075177391,'MPae!9%ufd',115899580476733,341.23 );")
	tk1.MustExec("insert into tbl_2 values ( '~udi',1839363347,'iQj$$YsZc5ULTxG)yH',111454353417190,6.6 );")
	tk1.MustExec("insert into tbl_3 values ( 'Alice',7032411265967085555,'P7*KBZ159','Alice',7516989 );")
	tk1.MustExec("insert into tbl_3 values ( 'David',486417871670147038,'','Charlie',-2135446 );")
	tk1.MustExec("insert into tbl_3 values ( 'Charlie',5784081664185069254,'7V_&YzKM~Q','Charlie',5583839 );")
	tk1.MustExec("insert into tbl_3 values ( 'David',6346366522897598558,')Lp&$2)SC@','Bob',2522913 );")
	tk1.MustExec("insert into tbl_3 values ( 'Charlie',224922711063053272,'gY','David',6624398 );")
	tk1.MustExec("insert into tbl_3 values ( 'Alice',4678579167560495958,'fPIXY%R8WyY(=u&O','David',-3267160 );")
	tk1.MustExec("insert into tbl_3 values ( 'David',8817108026311573677,'Cs0dZW*SPnKhV1','Alice',2359718 );")
	tk1.MustExec("insert into tbl_3 values ( 'Bob',3177426155683033662,'o2=@zv2qQDhKUs)4y','Bob',-8091802 );")
	tk1.MustExec("insert into tbl_3 values ( 'Bob',2543586640437235142,'hDa*CsOUzxmjf2m','Charlie',-8091935 );")
	tk1.MustExec("insert into tbl_3 values ( 'Charlie',6204182067887668945,'DX-!=)dbGPQO','David',-1954600 );")
	tk1.MustExec("insert into tbl_4 values ( 'David',167,576262750,'lX&x04W','2035-09-28' );")
	tk1.MustExec("insert into tbl_4 values ( 'Charlie',236,2637776757,'92OhsL!w%7','2036-02-08' );")
	tk1.MustExec("insert into tbl_4 values ( 'Bob',68,1077999933,'M0l','1997-09-16' );")
	tk1.MustExec("insert into tbl_4 values ( 'Charlie',184,1280264753,'FhjkfeXsK1Q(','2030-03-16' );")
	tk1.MustExec("insert into tbl_4 values ( 'Alice',10,2150711295,'Eqip)^tr*MoL','2032-07-02' );")
	tk1.MustExec("insert into tbl_4 values ( 'Bob',108,2421602476,'Eul~~Df_Q8s&I3Y-7','2019-06-10' );")
	tk1.MustExec("insert into tbl_4 values ( 'Alice',36,2811198561,'%XgRou0#iKtn*','2022-06-13' );")
	tk1.MustExec("insert into tbl_4 values ( 'Charlie',115,330972286,'hKeJS','2000-11-15' );")
	tk1.MustExec("insert into tbl_4 values ( 'Alice',6,2958326555,'c6+=1','2001-02-11' );")
	tk1.MustExec("insert into tbl_4 values ( 'Alice',99,387404826,'figc(@9R*k3!QM_Vve','2036-02-17' );")
	tk1.MustExec("insert into tbl_5 values ( -401358236474313609,'Charlie','4J$',701059766304691317,'08:19:10.00' );")
	tk1.MustExec("insert into tbl_5 values ( 2759837898825557143,'Bob','E',5158554038674310466,'11:04:03.00' );")
	tk1.MustExec("insert into tbl_5 values ( 273910054423832204,'Alice',null,8944547065167499612,'08:02:30.00' );")
	tk1.MustExec("insert into tbl_5 values ( 2875669873527090798,'Alice','4^SpR84',4072881341903432150,'18:24:55.00' );")
	tk1.MustExec("insert into tbl_5 values ( -8446590100588981557,'David','yBj8',8760380566452862549,'09:01:10.00' );")
	tk1.MustExec("insert into tbl_5 values ( -1075861460175889441,'Charlie','ti11Pl0lJ',9139997565676405627,'08:30:14.00' );")
	tk1.MustExec("insert into tbl_5 values ( 95663565223131772,'Alice','6$',8467839300407531400,'23:31:42.00' );")
	tk1.MustExec("insert into tbl_5 values ( -5661709703968335255,'Charlie','',8122758569495329946,'19:36:24.00' );")
	tk1.MustExec("insert into tbl_5 values ( 3338588216091909518,'Bob','',6558557574025196860,'15:22:56.00' );")
	tk1.MustExec("insert into tbl_5 values ( 8918630521194612922,'David','I$w',5981981639362947650,'22:03:24.00' );")
	tk1.MustExec("begin pessimistic;")
	tk1.MustExec("insert ignore into tbl_1 set col_1 = 'David', col_2 = 110, col_3 = 37065, col_4 = 8164500960513474805, col_5 = 1264 on duplicate key update col_3 = 22151.5, col_4 = 6266058887081523571, col_5 = 3254, col_2 = 59, col_1 = 'Bob';")
	tk1.MustExec("insert  into tbl_4 (col_16,col_17,col_18,col_19,col_20) values ( 'Charlie',34,2499970462,'Z','1978-10-27' ) ,( 'David',217,1732485689,'*)~@@Q8ryi','2004-12-01' ) ,( 'Charlie',40,1360558255,'H(Y','1998-06-25' ) ,( 'Alice',108,2973455447,'%CcP4$','1979-03-28' ) ,( 'David',9,3835209932,'tdKXUzLmAzwFf$','2009-03-03' ) ,( 'David',68,163270003,'uimsclz@FQJN','1988-09-11' ) ,( 'Alice',76,297067264,'BzFF','1989-01-05' ) on duplicate key update col_16 = 'Charlie', col_17 = 14, col_18 = 4062155275, col_20 = '2002-03-07', col_19 = 'tmvchLzp*o8';")
	tk2.MustExec("delete from tbl_3 where tbl_3.col_13 in ( null ,'' ,'g8EEzUU7LQ' ,'~fC3&B*cnOOx_' ,'%RF~AFto&x' ,'NlWkMWG^00' ,'e^4o2Ji^q_*Fa52Z' ) ;")
	tk2.MustExec("delete from tbl_5 where not( tbl_5.col_21 between -1075861460175889441 and 3338588216091909518 ) ;")
	tk1.MustExec("replace into tbl_1 (col_1,col_2,col_3,col_4,col_5) values ( 'Alice',83,8.33,4070808626051569664,455 ) ,( 'Alice',53,2.8,2763362085715461014,1912 ) ,( 'David',178,4242.8,962727993466011464,1844 ) ,( 'Alice',16,650054,5638988670318229867,565 ) ,( 'Alice',76,89783.1,3968605744540056024,2563 ) ,( 'Bob',120,0.89,1003144931151245839,2670 );")
	tk1.MustExec("delete from tbl_5 where col_24 is null ;")
	tk1.MustExec("delete from tbl_3 where tbl_3.col_11 in ( 'Alice' ,'Bob' ,'Alice' ) ;")
	tk2.MustExec("insert  into tbl_3 set col_11 = 'Bob', col_12 = 5701982550256146475, col_13 = 'Hhl)yCsQ2K3cfc^', col_14 = 'Alice', col_15 = -3718868 on duplicate key update col_15 = 7210750, col_12 = 6133680876296985245, col_14 = 'Alice', col_11 = 'David', col_13 = 'F+RMGE!_2^Cfr3Fw';")
	tk2.MustExec("insert ignore into tbl_5 set col_21 = 2439343116426563397, col_22 = 'Charlie', col_23 = '~Spa2YzRFFom16XD', col_24 = 5571575017340582365, col_25 = '13:24:38.00' ;")
	err := tk1.ExecToErr("update tbl_4 set tbl_4.col_20 = '2006-01-24' where tbl_4.col_18 in ( select col_11 from tbl_3 where IsNull( tbl_4.col_16 ) or not( tbl_4.col_19 in ( select col_3 from tbl_1 where tbl_4.col_16 between 'Alice' and 'David' and tbl_4.col_19 <= '%XgRou0#iKtn*' ) ) ) ;")
	if err != nil {
		print(err.Error())
		if strings.Contains(err.Error(), "Truncated incorrect DOUBLE value") {
			t.Log("Truncated incorrect DOUBLE value is within expectations, skipping")
			return
		}
	}
	require.NoError(t, err)
}

func TestOuterJoin(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1, t2, t3, t4")
	tk.MustExec("create table t1(a int, b int, c int)")
	tk.MustExec("create table t2(a int, b int, c int)")
	tk.MustExec("create table t3(a int, b int, c int)")
	tk.MustExec("create table t4(a int, b int, c int)")
	tk.MustExec("INSERT INTO t1 VALUES (1,3,0), (2,2,0), (3,2,0);")
	tk.MustExec("INSERT INTO t2 VALUES (3,3,0), (4,2,0), (5,3,0);")
	tk.MustExec("INSERT INTO t3 VALUES (1,2,0), (2,2,0);")
	tk.MustExec("INSERT INTO t4 VALUES (3,2,0), (4,2,0);")
	tk.MustQuery("SELECT t2.a,t2.b,t3.a,t3.b,t4.a,t4.b from (t3, t4) left join (t1, t2) on t3.a=1 AND t3.b=t2.b AND t2.b=t4.b order by 1, 2, 3, 4, 5;").Check(
		testkit.Rows(
			"<nil> <nil> 2 2 3 2",
			"<nil> <nil> 2 2 4 2",
			"4 2 1 2 3 2",
			"4 2 1 2 3 2",
			"4 2 1 2 3 2",
			"4 2 1 2 4 2",
			"4 2 1 2 4 2",
			"4 2 1 2 4 2",
		),
	)

	tk.MustExec("drop table if exists t1, t2, t3")
	tk.MustExec("create table t1 (a1 int, a2 int);")
	tk.MustExec("create table t2 (b1 int not null, b2 int);")
	tk.MustExec("create table t3 (c1 int, c2 int);")
	tk.MustExec("insert into t1 values (1,2), (2,2), (3,2);")
	tk.MustExec("insert into t2 values (1,3), (2,3);")
	tk.MustExec("insert into t3 values (2,4),        (3,4);")
	tk.MustQuery("select * from t1 left join t2  on  b1 = a1 left join t3  on  c1 = a1  and  b1 is null order by 1, 2, 3, 4, 5, 6").Check(
		testkit.Rows(
			"1 2 1 3 <nil> <nil>",
			"2 2 2 3 <nil> <nil>",
			"3 2 <nil> <nil> 3 4",
		),
	)
}
