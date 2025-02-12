// Copyright 2015 PingCAP, Inc.
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

package ddl

import (
	"context"
	"time"

	. "github.com/pingcap/check"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/meta"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/table/tables"
	"github.com/pingcap/tidb/types"
)

type testCtxKeyType int

func (k testCtxKeyType) String() string {
	return "test_ctx_key"
}

const testCtxKey testCtxKeyType = 0

func (s *testDDLSuite) TestReorg(c *C) {
	store := testCreateStore(c, "test_reorg")
	defer func() {
		err := store.Close()
		c.Assert(err, IsNil)
	}()

	d, err := testNewDDLAndStart(
		context.Background(),
		WithStore(store),
		WithLease(testLease),
	)
	c.Assert(err, IsNil)
	defer func() {
		err := d.Stop()
		c.Assert(err, IsNil)
	}()

	time.Sleep(testLease)

	ctx := testNewContext(d)

	ctx.SetValue(testCtxKey, 1)
	c.Assert(ctx.Value(testCtxKey), Equals, 1)
	ctx.ClearValue(testCtxKey)

	err = ctx.NewTxn(context.Background())
	c.Assert(err, IsNil)
	txn, err := ctx.Txn(true)
	c.Assert(err, IsNil)
	err = txn.Set([]byte("a"), []byte("b"))
	c.Assert(err, IsNil)
	err = txn.Rollback()
	c.Assert(err, IsNil)

	err = ctx.NewTxn(context.Background())
	c.Assert(err, IsNil)
	txn, err = ctx.Txn(true)
	c.Assert(err, IsNil)
	err = txn.Set([]byte("a"), []byte("b"))
	c.Assert(err, IsNil)
	err = txn.Commit(context.Background())
	c.Assert(err, IsNil)

	rowCount := int64(10)
	handle := s.NewHandle().Int(100).Common("a", 100, "string")
	f := func() error {
		d.generalWorker().reorgCtx.setRowCount(rowCount)
		d.generalWorker().reorgCtx.setNextKey(handle.Encoded())
		time.Sleep(1*ReorgWaitTimeout + 100*time.Millisecond)
		return nil
	}
	job := &model.Job{
		ID:          1,
		SnapshotVer: 1, // Make sure it is not zero. So the reorgInfo's first is false.
	}
	err = ctx.NewTxn(context.Background())
	c.Assert(err, IsNil)
	txn, err = ctx.Txn(true)
	c.Assert(err, IsNil)
	m := meta.NewMeta(txn)
	e := &meta.Element{ID: 333, TypeKey: meta.IndexElementKey}
	rInfo := &reorgInfo{
		Job:         job,
		currElement: e,
	}
	mockTbl := tables.MockTableFromMeta(&model.TableInfo{IsCommonHandle: s.IsCommonHandle, CommonHandleVersion: 1})
	err = d.generalWorker().runReorgJob(m, rInfo, mockTbl.Meta(), d.lease, f)
	c.Assert(err, NotNil)

	// The longest to wait for 5 seconds to make sure the function of f is returned.
	for i := 0; i < 1000; i++ {
		time.Sleep(5 * time.Millisecond)
		err = d.generalWorker().runReorgJob(m, rInfo, mockTbl.Meta(), d.lease, f)
		if err == nil {
			c.Assert(job.RowCount, Equals, rowCount)
			c.Assert(d.generalWorker().reorgCtx.rowCount, Equals, int64(0))

			// Test whether reorgInfo's Handle is update.
			err = txn.Commit(context.Background())
			c.Assert(err, IsNil)
			err = ctx.NewTxn(context.Background())
			c.Assert(err, IsNil)

			m = meta.NewMeta(txn)
			info, err1 := getReorgInfo(d.ddlCtx, m, job, mockTbl, nil)
			c.Assert(err1, IsNil)
			c.Assert(info.StartKey, DeepEquals, kv.Key(handle.Encoded()))
			c.Assert(info.currElement, DeepEquals, e)
			_, doneHandle, _ := d.generalWorker().reorgCtx.getRowCountAndKey()
			c.Assert(doneHandle, IsNil)
			break
		}
	}
	c.Assert(err, IsNil)

	job = &model.Job{
		ID:          2,
		SchemaID:    1,
		Type:        model.ActionCreateSchema,
		Args:        []interface{}{model.NewCIStr("test")},
		SnapshotVer: 1, // Make sure it is not zero. So the reorgInfo's first is false.
	}

	element := &meta.Element{ID: 123, TypeKey: meta.ColumnElementKey}
	info := &reorgInfo{
		Job:             job,
		d:               d.ddlCtx,
		currElement:     element,
		StartKey:        s.NewHandle().Int(1).Common(100, "string").Encoded(),
		EndKey:          s.NewHandle().Int(0).Common(101, "string").Encoded(),
		PhysicalTableID: 456,
	}
	err = kv.RunInNewTxn(context.Background(), d.store, false, func(ctx context.Context, txn kv.Transaction) error {
		t := meta.NewMeta(txn)
		var err1 error
		_, err1 = getReorgInfo(d.ddlCtx, t, job, mockTbl, []*meta.Element{element})
		c.Assert(meta.ErrDDLReorgElementNotExist.Equal(err1), IsTrue)
		c.Assert(job.SnapshotVer, Equals, uint64(0))
		return nil
	})
	c.Assert(err, IsNil)
	job.SnapshotVer = uint64(1)
	err = info.UpdateReorgMeta(info.StartKey)
	c.Assert(err, IsNil)
	err = kv.RunInNewTxn(context.Background(), d.store, false, func(ctx context.Context, txn kv.Transaction) error {
		t := meta.NewMeta(txn)
		info1, err1 := getReorgInfo(d.ddlCtx, t, job, mockTbl, []*meta.Element{element})
		c.Assert(err1, IsNil)
		c.Assert(info1.currElement, DeepEquals, info.currElement)
		c.Assert(info1.StartKey, DeepEquals, info.StartKey)
		c.Assert(info1.EndKey, DeepEquals, info.EndKey)
		c.Assert(info1.PhysicalTableID, Equals, info.PhysicalTableID)
		return nil
	})
	c.Assert(err, IsNil)

	err = d.Stop()
	c.Assert(err, IsNil)
	err = d.generalWorker().runReorgJob(m, rInfo, mockTbl.Meta(), d.lease, func() error {
		time.Sleep(4 * testLease)
		return nil
	})
	c.Assert(err, NotNil)
	txn, err = ctx.Txn(true)
	c.Assert(err, IsNil)
	err = txn.Commit(context.Background())
	c.Assert(err, IsNil)
	s.RerunWithCommonHandleEnabled(c, s.TestReorg)
}

func (s *testDDLSuite) TestReorgOwner(c *C) {
	store := testCreateStore(c, "test_reorg_owner")
	defer func() {
		err := store.Close()
		c.Assert(err, IsNil)
	}()

	d1, err := testNewDDLAndStart(
		context.Background(),
		WithStore(store),
		WithLease(testLease),
	)
	c.Assert(err, IsNil)
	defer func() {
		err := d1.Stop()
		c.Assert(err, IsNil)
	}()

	ctx := testNewContext(d1)

	testCheckOwner(c, d1, true)

	d2, err := testNewDDLAndStart(
		context.Background(),
		WithStore(store),
		WithLease(testLease),
	)
	c.Assert(err, IsNil)
	defer func() {
		err := d2.Stop()
		c.Assert(err, IsNil)
	}()

	dbInfo, err := testSchemaInfo(d1, "test_reorg")
	c.Assert(err, IsNil)
	testCreateSchema(c, ctx, d1, dbInfo)

	tblInfo := testTableInfo(c, d1, "t", 3)
	testCreateTable(c, ctx, d1, dbInfo, tblInfo)
	t := testGetTable(c, d1, dbInfo.ID, tblInfo.ID)

	num := 10
	for i := 0; i < num; i++ {
		_, err := t.AddRecord(ctx, types.MakeDatums(i, i, i))
		c.Assert(err, IsNil)
	}

	txn, err := ctx.Txn(true)
	c.Assert(err, IsNil)
	err = txn.Commit(context.Background())
	c.Assert(err, IsNil)

	tc := &TestDDLCallback{}
	tc.onJobRunBefore = func(job *model.Job) {
		if job.SchemaState == model.StateDeleteReorganization {
			err = d1.Stop()
			c.Assert(err, IsNil)
		}
	}

	d1.SetHook(tc)

	testDropSchema(c, ctx, d1, dbInfo)

	err = kv.RunInNewTxn(context.Background(), d1.store, false, func(ctx context.Context, txn kv.Transaction) error {
		t := meta.NewMeta(txn)
		db, err1 := t.GetDatabase(dbInfo.ID)
		c.Assert(err1, IsNil)
		c.Assert(db, IsNil)
		return nil
	})
	c.Assert(err, IsNil)
}
