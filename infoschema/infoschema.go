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

package infoschema

import (
	"fmt"
	"sort"
	"sync"

	"github.com/pingcap/tidb/ddl/placement"
	"github.com/pingcap/tidb/meta/autoid"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/parser/mysql"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/mock"
	"golang.org/x/exp/slices"
)

// InfoSchema is the interface used to retrieve the schema information.
// It works as a in memory cache and doesn't handle any schema change.
// InfoSchema is read-only, and the returned value is a copy.
// TODO: add more methods to retrieve tables and columns.
type InfoSchema interface {
	SchemaByName(schema model.CIStr) (*model.DBInfo, bool)
	SchemaExists(schema model.CIStr) bool
	TableByName(schema, table model.CIStr) (table.Table, error)
	TableExists(schema, table model.CIStr) bool
	SchemaByID(id int64) (*model.DBInfo, bool)
	SchemaByTable(tableInfo *model.TableInfo) (*model.DBInfo, bool)
	PolicyByName(name model.CIStr) (*model.PolicyInfo, bool)
	TableByID(id int64) (table.Table, bool)
	AllocByID(id int64) (autoid.Allocators, bool)
	AllSchemaNames() []string
	AllSchemas() []*model.DBInfo
	Clone() (result []*model.DBInfo)
	SchemaTables(schema model.CIStr) []table.Table
	SchemaMetaVersion() int64
	// TableIsView indicates whether the schema.table is a view.
	TableIsView(schema, table model.CIStr) bool
	// TableIsSequence indicates whether the schema.table is a sequence.
	TableIsSequence(schema, table model.CIStr) bool
	FindTableByPartitionID(partitionID int64) (table.Table, *model.DBInfo, *model.PartitionDefinition)
	// PlacementBundleByPhysicalTableID is used to get a rule bundle.
	PlacementBundleByPhysicalTableID(id int64) (*placement.Bundle, bool)
	// AllPlacementBundles is used to get all placement bundles
	AllPlacementBundles() []*placement.Bundle
	// AllPlacementPolicies returns all placement policies
	AllPlacementPolicies() []*model.PolicyInfo
	// HasTemporaryTable returns whether information schema has temporary table
	HasTemporaryTable() bool
	// GetTableReferredForeignKeys gets the table's ReferredFKInfo by lowercase schema and table name.
	GetTableReferredForeignKeys(schema, table string) []*model.ReferredFKInfo
}

type sortedTables []table.Table

func (s sortedTables) searchTable(id int64) int {
	idx := sort.Search(len(s), func(i int) bool {
		return s[i].Meta().ID >= id
	})
	if idx == len(s) || s[idx].Meta().ID != id {
		return -1
	}
	return idx
}

type schemaTables struct {
	dbInfo *model.DBInfo
	tables map[string]table.Table
}

const bucketCount = 512

type infoSchema struct {
	// ruleBundleMap stores all placement rules
	ruleBundleMap map[int64]*placement.Bundle

	// policyMap stores all placement policies.
	policyMutex sync.RWMutex
	policyMap   map[string]*model.PolicyInfo

	schemaMap map[string]*schemaTables

	// sortedTablesBuckets is a slice of sortedTables, a table's bucket index is (tableID % bucketCount).
	sortedTablesBuckets []sortedTables

	// temporaryTables stores the temporary table ids
	temporaryTableIDs map[int64]struct{}

	// schemaMetaVersion is the version of schema, and we should check version when change schema.
	schemaMetaVersion int64

	// referredForeignKeyMap records all table's ReferredFKInfo.
	// referredSchemaAndTableName => child SchemaAndTableAndForeignKeyName => *model.ReferredFKInfo
	referredForeignKeyMap map[SchemaAndTableName][]*model.ReferredFKInfo
}

// SchemaAndTableName contains the lower-case schema name and table name.
type SchemaAndTableName struct {
	schema string
	table  string
}

// MockInfoSchema only serves for test.
func MockInfoSchema(tbList []*model.TableInfo) InfoSchema {
	result := &infoSchema{}
	result.schemaMap = make(map[string]*schemaTables)
	result.policyMap = make(map[string]*model.PolicyInfo)
	result.ruleBundleMap = make(map[int64]*placement.Bundle)
	result.sortedTablesBuckets = make([]sortedTables, bucketCount)
	dbInfo := &model.DBInfo{ID: 0, Name: model.NewCIStr("test"), Tables: tbList}
	tableNames := &schemaTables{
		dbInfo: dbInfo,
		tables: make(map[string]table.Table),
	}
	result.schemaMap["test"] = tableNames
	for _, tb := range tbList {
		tbl := table.MockTableFromMeta(tb)
		tableNames.tables[tb.Name.L] = tbl
		bucketIdx := tableBucketIdx(tb.ID)
		result.sortedTablesBuckets[bucketIdx] = append(result.sortedTablesBuckets[bucketIdx], tbl)
	}
	for i := range result.sortedTablesBuckets {
		slices.SortFunc(result.sortedTablesBuckets[i], func(i, j table.Table) bool {
			return i.Meta().ID < j.Meta().ID
		})
	}
	return result
}

// MockInfoSchemaWithSchemaVer only serves for test.
func MockInfoSchemaWithSchemaVer(tbList []*model.TableInfo, schemaVer int64) InfoSchema {
	result := &infoSchema{}
	result.schemaMap = make(map[string]*schemaTables)
	result.policyMap = make(map[string]*model.PolicyInfo)
	result.ruleBundleMap = make(map[int64]*placement.Bundle)
	result.sortedTablesBuckets = make([]sortedTables, bucketCount)
	dbInfo := &model.DBInfo{ID: 0, Name: model.NewCIStr("test"), Tables: tbList}
	tableNames := &schemaTables{
		dbInfo: dbInfo,
		tables: make(map[string]table.Table),
	}
	result.schemaMap["test"] = tableNames
	for _, tb := range tbList {
		tbl := table.MockTableFromMeta(tb)
		tableNames.tables[tb.Name.L] = tbl
		bucketIdx := tableBucketIdx(tb.ID)
		result.sortedTablesBuckets[bucketIdx] = append(result.sortedTablesBuckets[bucketIdx], tbl)
	}
	for i := range result.sortedTablesBuckets {
		slices.SortFunc(result.sortedTablesBuckets[i], func(i, j table.Table) bool {
			return i.Meta().ID < j.Meta().ID
		})
	}
	result.schemaMetaVersion = schemaVer
	return result
}

var _ InfoSchema = (*infoSchema)(nil)

func (is *infoSchema) SchemaByName(schema model.CIStr) (val *model.DBInfo, ok bool) {
	tableNames, ok := is.schemaMap[schema.L]
	if !ok {
		return
	}
	return tableNames.dbInfo, true
}

func (is *infoSchema) SchemaMetaVersion() int64 {
	return is.schemaMetaVersion
}

func (is *infoSchema) SchemaExists(schema model.CIStr) bool {
	_, ok := is.schemaMap[schema.L]
	return ok
}

func (is *infoSchema) TableByName(schema, table model.CIStr) (t table.Table, err error) {
	if tbNames, ok := is.schemaMap[schema.L]; ok {
		if t, ok = tbNames.tables[table.L]; ok {
			return
		}
	}
	return nil, ErrTableNotExists.GenWithStackByArgs(schema, table)
}

func (is *infoSchema) TableIsView(schema, table model.CIStr) bool {
	if tbNames, ok := is.schemaMap[schema.L]; ok {
		if t, ok := tbNames.tables[table.L]; ok {
			return t.Meta().IsView()
		}
	}
	return false
}

func (is *infoSchema) TableIsSequence(schema, table model.CIStr) bool {
	if tbNames, ok := is.schemaMap[schema.L]; ok {
		if t, ok := tbNames.tables[table.L]; ok {
			return t.Meta().IsSequence()
		}
	}
	return false
}

func (is *infoSchema) TableExists(schema, table model.CIStr) bool {
	if tbNames, ok := is.schemaMap[schema.L]; ok {
		if _, ok = tbNames.tables[table.L]; ok {
			return true
		}
	}
	return false
}

func (is *infoSchema) PolicyByID(id int64) (val *model.PolicyInfo, ok bool) {
	// TODO: use another hash map to avoid traveling on the policy map
	for _, v := range is.policyMap {
		if v.ID == id {
			return v, true
		}
	}
	return nil, false
}

func (is *infoSchema) SchemaByID(id int64) (val *model.DBInfo, ok bool) {
	for _, v := range is.schemaMap {
		if v.dbInfo.ID == id {
			return v.dbInfo, true
		}
	}
	return nil, false
}

func (is *infoSchema) SchemaByTable(tableInfo *model.TableInfo) (val *model.DBInfo, ok bool) {
	if tableInfo == nil {
		return nil, false
	}
	for _, v := range is.schemaMap {
		if tbl, ok := v.tables[tableInfo.Name.L]; ok {
			if tbl.Meta().ID == tableInfo.ID {
				return v.dbInfo, true
			}
		}
	}
	return nil, false
}

func (is *infoSchema) TableByID(id int64) (val table.Table, ok bool) {
	slice := is.sortedTablesBuckets[tableBucketIdx(id)]
	idx := slice.searchTable(id)
	if idx == -1 {
		return nil, false
	}
	return slice[idx], true
}

func (is *infoSchema) AllocByID(id int64) (autoid.Allocators, bool) {
	tbl, ok := is.TableByID(id)
	if !ok {
		return nil, false
	}
	return tbl.Allocators(nil), true
}

func (is *infoSchema) AllSchemaNames() (names []string) {
	for _, v := range is.schemaMap {
		names = append(names, v.dbInfo.Name.O)
	}
	return
}

func (is *infoSchema) AllSchemas() (schemas []*model.DBInfo) {
	for _, v := range is.schemaMap {
		schemas = append(schemas, v.dbInfo)
	}
	return
}

func (is *infoSchema) SchemaTables(schema model.CIStr) (tables []table.Table) {
	schemaTables, ok := is.schemaMap[schema.L]
	if !ok {
		return
	}
	for _, tbl := range schemaTables.tables {
		tables = append(tables, tbl)
	}
	return
}

// FindTableByPartitionID finds the partition-table info by the partitionID.
// FindTableByPartitionID will traverse all the tables to find the partitionID partition in which partition-table.
func (is *infoSchema) FindTableByPartitionID(partitionID int64) (table.Table, *model.DBInfo, *model.PartitionDefinition) {
	for _, v := range is.schemaMap {
		for _, tbl := range v.tables {
			pi := tbl.Meta().GetPartitionInfo()
			if pi == nil {
				continue
			}
			for _, p := range pi.Definitions {
				if p.ID == partitionID {
					return tbl, v.dbInfo, &p
				}
			}
		}
	}
	return nil, nil, nil
}

// HasTemporaryTable returns whether information schema has temporary table
func (is *infoSchema) HasTemporaryTable() bool {
	return len(is.temporaryTableIDs) != 0
}

func (is *infoSchema) Clone() (result []*model.DBInfo) {
	for _, v := range is.schemaMap {
		result = append(result, v.dbInfo.Clone())
	}
	return
}

// GetSequenceByName gets the sequence by name.
func GetSequenceByName(is InfoSchema, schema, sequence model.CIStr) (util.SequenceTable, error) {
	tbl, err := is.TableByName(schema, sequence)
	if err != nil {
		return nil, err
	}
	if !tbl.Meta().IsSequence() {
		return nil, ErrWrongObject.GenWithStackByArgs(schema, sequence, "SEQUENCE")
	}
	return tbl.(util.SequenceTable), nil
}

func init() {
	// Initialize the information shema database and register the driver to `drivers`
	dbID := autoid.InformationSchemaDBID
	infoSchemaTables := make([]*model.TableInfo, 0, len(tableNameToColumns))
	for name, cols := range tableNameToColumns {
		tableInfo := buildTableMeta(name, cols)
		infoSchemaTables = append(infoSchemaTables, tableInfo)
		var ok bool
		tableInfo.ID, ok = tableIDMap[tableInfo.Name.O]
		if !ok {
			panic(fmt.Sprintf("get information_schema table id failed, unknown system table `%v`", tableInfo.Name.O))
		}
		for i, c := range tableInfo.Columns {
			c.ID = int64(i) + 1
		}
	}
	infoSchemaDB := &model.DBInfo{
		ID:      dbID,
		Name:    util.InformationSchemaName,
		Charset: mysql.DefaultCharset,
		Collate: mysql.DefaultCollationName,
		Tables:  infoSchemaTables,
	}
	RegisterVirtualTable(infoSchemaDB, createInfoSchemaTable)
	util.GetSequenceByName = func(is interface{}, schema, sequence model.CIStr) (util.SequenceTable, error) {
		return GetSequenceByName(is.(InfoSchema), schema, sequence)
	}
	mock.MockInfoschema = func(tbList []*model.TableInfo) sessionctx.InfoschemaMetaVersion {
		return MockInfoSchema(tbList)
	}
}

// HasAutoIncrementColumn checks whether the table has auto_increment columns, if so, return true and the column name.
func HasAutoIncrementColumn(tbInfo *model.TableInfo) (bool, string) {
	for _, col := range tbInfo.Columns {
		if mysql.HasAutoIncrementFlag(col.GetFlag()) {
			return true, col.Name.L
		}
	}
	return false, ""
}

// PolicyByName is used to find the policy.
func (is *infoSchema) PolicyByName(name model.CIStr) (*model.PolicyInfo, bool) {
	is.policyMutex.RLock()
	defer is.policyMutex.RUnlock()
	t, r := is.policyMap[name.L]
	return t, r
}

// AllPlacementPolicies returns all placement policies
func (is *infoSchema) AllPlacementPolicies() []*model.PolicyInfo {
	is.policyMutex.RLock()
	defer is.policyMutex.RUnlock()
	policies := make([]*model.PolicyInfo, 0, len(is.policyMap))
	for _, policy := range is.policyMap {
		policies = append(policies, policy)
	}
	return policies
}

func (is *infoSchema) PlacementBundleByPhysicalTableID(id int64) (*placement.Bundle, bool) {
	t, r := is.ruleBundleMap[id]
	return t, r
}

func (is *infoSchema) AllPlacementBundles() []*placement.Bundle {
	bundles := make([]*placement.Bundle, 0, len(is.ruleBundleMap))
	for _, bundle := range is.ruleBundleMap {
		bundles = append(bundles, bundle)
	}
	return bundles
}

func (is *infoSchema) setPolicy(policy *model.PolicyInfo) {
	is.policyMutex.Lock()
	defer is.policyMutex.Unlock()
	is.policyMap[policy.Name.L] = policy
}

func (is *infoSchema) deletePolicy(name string) {
	is.policyMutex.Lock()
	defer is.policyMutex.Unlock()
	delete(is.policyMap, name)
}

func (is *infoSchema) addReferredForeignKeys(schema model.CIStr, tbInfo *model.TableInfo) {
	for _, fk := range tbInfo.ForeignKeys {
		if fk.Version < model.FKVersion1 {
			continue
		}
		refer := SchemaAndTableName{schema: fk.RefSchema.L, table: fk.RefTable.L}
		referredFKList := is.referredForeignKeyMap[refer]
		found := false
		for _, referredFK := range referredFKList {
			if referredFK.ChildSchema.L == schema.L && referredFK.ChildTable.L == tbInfo.Name.L && referredFK.ChildFKName.L == fk.Name.L {
				referredFK.Cols = fk.RefCols
				found = true
				break
			}
		}
		if found {
			continue
		}

		newReferredFKList := make([]*model.ReferredFKInfo, 0, len(referredFKList)+1)
		newReferredFKList = append(newReferredFKList, referredFKList...)
		newReferredFKList = append(newReferredFKList, &model.ReferredFKInfo{
			Cols:        fk.RefCols,
			ChildSchema: schema,
			ChildTable:  tbInfo.Name,
			ChildFKName: fk.Name,
		})
		sort.Slice(newReferredFKList, func(i, j int) bool {
			if newReferredFKList[i].ChildSchema.L != newReferredFKList[j].ChildSchema.L {
				return newReferredFKList[i].ChildSchema.L < newReferredFKList[j].ChildSchema.L
			}
			if newReferredFKList[i].ChildTable.L != newReferredFKList[j].ChildTable.L {
				return newReferredFKList[i].ChildTable.L < newReferredFKList[j].ChildTable.L
			}
			return newReferredFKList[i].ChildFKName.L < newReferredFKList[j].ChildFKName.L
		})
		is.referredForeignKeyMap[refer] = newReferredFKList
	}
}

func (is *infoSchema) deleteReferredForeignKeys(schema model.CIStr, tbInfo *model.TableInfo) {
	for _, fk := range tbInfo.ForeignKeys {
		if fk.Version < model.FKVersion1 {
			continue
		}
		refer := SchemaAndTableName{schema: fk.RefSchema.L, table: fk.RefTable.L}
		referredFKList := is.referredForeignKeyMap[refer]
		if len(referredFKList) == 0 {
			continue
		}
		newReferredFKList := make([]*model.ReferredFKInfo, 0, len(referredFKList)-1)
		for _, referredFK := range referredFKList {
			if referredFK.ChildSchema.L == schema.L && referredFK.ChildTable.L == tbInfo.Name.L && referredFK.ChildFKName.L == fk.Name.L {
				continue
			}
			newReferredFKList = append(newReferredFKList, referredFK)
		}
		is.referredForeignKeyMap[refer] = newReferredFKList
	}
}

// GetTableReferredForeignKeys gets the table's ReferredFKInfo by lowercase schema and table name.
func (is *infoSchema) GetTableReferredForeignKeys(schema, table string) []*model.ReferredFKInfo {
	name := SchemaAndTableName{schema: schema, table: table}
	return is.referredForeignKeyMap[name]
}

// SessionTables store local temporary tables
type SessionTables struct {
	// Session tables can be accessed after the db is dropped, so there needs a way to retain the DBInfo.
	// schemaTables.dbInfo will only be used when the db is dropped and it may be stale after the db is created again.
	// But it's fine because we only need its name.
	schemaMap map[string]*schemaTables
	idx2table map[int64]table.Table
}

// NewSessionTables creates a new NewSessionTables object
func NewSessionTables() *SessionTables {
	return &SessionTables{
		schemaMap: make(map[string]*schemaTables),
		idx2table: make(map[int64]table.Table),
	}
}

// TableByName get table by name
func (is *SessionTables) TableByName(schema, table model.CIStr) (table.Table, bool) {
	if tbNames, ok := is.schemaMap[schema.L]; ok {
		if t, ok := tbNames.tables[table.L]; ok {
			return t, true
		}
	}
	return nil, false
}

// TableExists check if table with the name exists
func (is *SessionTables) TableExists(schema, table model.CIStr) (ok bool) {
	_, ok = is.TableByName(schema, table)
	return
}

// TableByID get table by table id
func (is *SessionTables) TableByID(id int64) (tbl table.Table, ok bool) {
	tbl, ok = is.idx2table[id]
	return
}

// AddTable add a table
func (is *SessionTables) AddTable(db *model.DBInfo, tbl table.Table) error {
	schemaTables := is.ensureSchema(db)

	tblMeta := tbl.Meta()
	if _, ok := schemaTables.tables[tblMeta.Name.L]; ok {
		return ErrTableExists.GenWithStackByArgs(tblMeta.Name)
	}

	if _, ok := is.idx2table[tblMeta.ID]; ok {
		return ErrTableExists.GenWithStackByArgs(tblMeta.Name)
	}

	schemaTables.tables[tblMeta.Name.L] = tbl
	is.idx2table[tblMeta.ID] = tbl

	return nil
}

// RemoveTable remove a table
func (is *SessionTables) RemoveTable(schema, table model.CIStr) (exist bool) {
	tbls := is.schemaTables(schema)
	if tbls == nil {
		return false
	}

	oldTable, exist := tbls.tables[table.L]
	if !exist {
		return false
	}

	delete(tbls.tables, table.L)
	delete(is.idx2table, oldTable.Meta().ID)
	if len(tbls.tables) == 0 {
		delete(is.schemaMap, schema.L)
	}
	return true
}

// Count gets the count of the temporary tables.
func (is *SessionTables) Count() int {
	return len(is.idx2table)
}

// SchemaByTable get a table's schema name
func (is *SessionTables) SchemaByTable(tableInfo *model.TableInfo) (*model.DBInfo, bool) {
	if tableInfo == nil {
		return nil, false
	}

	for _, v := range is.schemaMap {
		if tbl, ok := v.tables[tableInfo.Name.L]; ok {
			if tbl.Meta().ID == tableInfo.ID {
				return v.dbInfo, true
			}
		}
	}

	return nil, false
}

func (is *SessionTables) ensureSchema(db *model.DBInfo) *schemaTables {
	if tbls, ok := is.schemaMap[db.Name.L]; ok {
		return tbls
	}

	tbls := &schemaTables{dbInfo: db, tables: make(map[string]table.Table)}
	is.schemaMap[db.Name.L] = tbls
	return tbls
}

func (is *SessionTables) schemaTables(schema model.CIStr) *schemaTables {
	if is.schemaMap == nil {
		return nil
	}

	if tbls, ok := is.schemaMap[schema.L]; ok {
		return tbls
	}

	return nil
}

// SessionExtendedInfoSchema implements InfoSchema
// Local temporary table has a loose relationship with database.
// So when a database is dropped, its temporary tables still exist and can be returned by TableByName/TableByID.
type SessionExtendedInfoSchema struct {
	InfoSchema
	LocalTemporaryTablesOnce sync.Once
	LocalTemporaryTables     *SessionTables
	MdlTables                *SessionTables
}

// TableByName implements InfoSchema.TableByName
func (ts *SessionExtendedInfoSchema) TableByName(schema, table model.CIStr) (table.Table, error) {
	if ts.LocalTemporaryTables != nil {
		if tbl, ok := ts.LocalTemporaryTables.TableByName(schema, table); ok {
			return tbl, nil
		}
	}

	if ts.MdlTables != nil {
		if tbl, ok := ts.MdlTables.TableByName(schema, table); ok {
			return tbl, nil
		}
	}

	return ts.InfoSchema.TableByName(schema, table)
}

// TableByID implements InfoSchema.TableByID
func (ts *SessionExtendedInfoSchema) TableByID(id int64) (table.Table, bool) {
	if ts.LocalTemporaryTables != nil {
		if tbl, ok := ts.LocalTemporaryTables.TableByID(id); ok {
			return tbl, true
		}
	}

	if ts.MdlTables != nil {
		if tbl, ok := ts.MdlTables.TableByID(id); ok {
			return tbl, true
		}
	}

	return ts.InfoSchema.TableByID(id)
}

// SchemaByTable implements InfoSchema.SchemaByTable, it returns a stale DBInfo even if it's dropped.
func (ts *SessionExtendedInfoSchema) SchemaByTable(tableInfo *model.TableInfo) (*model.DBInfo, bool) {
	if tableInfo == nil {
		return nil, false
	}

	if ts.LocalTemporaryTables != nil {
		if db, ok := ts.LocalTemporaryTables.SchemaByTable(tableInfo); ok {
			return db, true
		}
	}

	return ts.InfoSchema.SchemaByTable(tableInfo)
}

// UpdateTableInfo implements InfoSchema.SchemaByTable.
func (ts *SessionExtendedInfoSchema) UpdateTableInfo(db *model.DBInfo, tableInfo table.Table) error {
	if ts.MdlTables == nil {
		ts.MdlTables = NewSessionTables()
	}
	err := ts.MdlTables.AddTable(db, tableInfo)
	if err != nil {
		return err
	}
	return nil
}

// HasTemporaryTable returns whether information schema has temporary table
func (ts *SessionExtendedInfoSchema) HasTemporaryTable() bool {
	return ts.LocalTemporaryTables != nil && ts.LocalTemporaryTables.Count() > 0 || ts.InfoSchema.HasTemporaryTable()
}

// AttachMDLTableInfoSchema attach MDL related table information schema to is
func AttachMDLTableInfoSchema(is InfoSchema) InfoSchema {
	mdlTables := NewSessionTables()
	if iss, ok := is.(*SessionExtendedInfoSchema); ok {
		iss.MdlTables = mdlTables
		return iss
	}
	return &SessionExtendedInfoSchema{
		InfoSchema: is,
		MdlTables:  mdlTables,
	}
}
