// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package mysql

import (
	"encoding/hex"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"ariga.io/atlas/sql/internal/sqlx"
	"ariga.io/atlas/sql/schema"
)

// A diff provides a MySQL implementation for sqlx.DiffDriver.
type diff struct{ conn }

// SchemaAttrDiff returns a changeset for migrating schema attributes from one state to the other.
func (d *diff) SchemaAttrDiff(from, to *schema.Schema) []schema.Change {
	var changes []schema.Change
	// Charset change.
	if change := d.charsetChange(from.Attrs, from.Realm.Attrs, to.Attrs); change != noChange {
		changes = append(changes, change)
	}
	// Collation change.
	if change := d.collationChange(from.Attrs, from.Realm.Attrs, to.Attrs); change != noChange {
		changes = append(changes, change)
	}
	return changes
}

// TableAttrDiff returns a changeset for migrating table attributes from one state to the other.
func (d *diff) TableAttrDiff(from, to *schema.Table) ([]schema.Change, error) {
	var changes []schema.Change
	if change := d.autoIncChange(from.Attrs, to.Attrs); change != noChange {
		changes = append(changes, change)
	}
	if change := sqlx.CommentDiff(from.Attrs, to.Attrs); change != nil {
		changes = append(changes, change)
	}
	if change := d.charsetChange(from.Attrs, from.Schema.Attrs, to.Attrs); change != noChange {
		changes = append(changes, change)
	}
	if change := d.collationChange(from.Attrs, from.Schema.Attrs, to.Attrs); change != noChange {
		changes = append(changes, change)
	}
	if _, ok := d.supportsCheck(); !ok && sqlx.Has(to.Attrs, &schema.Check{}) {
		return nil, fmt.Errorf("version %q does not support CHECK constraints", d.version)
	}
	// For MariaDB, we skip JSON CHECK constraints that were created by the databases,
	// or by Atlas for older versions. These CHECK constraints (inlined on the columns)
	// also cannot be dropped using "DROP CONSTRAINTS", but can be modified and dropped
	// using "MODIFY COLUMN".
	var checks []schema.Change
	for _, c := range sqlx.CheckDiff(from, to, func(c1, c2 *schema.Check) bool {
		return enforced(c1.Attrs) == enforced(c2.Attrs)
	}) {
		drop, ok := c.(*schema.DropCheck)
		if !ok || !strings.HasPrefix(drop.C.Expr, "json_valid") {
			checks = append(checks, c)
			continue
		}
		// Generated CHECK have the form of "json_valid(`<column>`)"
		// and named as the column.
		if _, ok := to.Column(drop.C.Name); !ok {
			checks = append(checks, c)
		}
	}
	return append(changes, checks...), nil
}

// ColumnChange returns the schema changes (if any) for migrating one column to the other.
func (d *diff) ColumnChange(from, to *schema.Column) (schema.ChangeKind, error) {
	change := sqlx.CommentChange(from.Attrs, to.Attrs)
	if from.Type.Null != to.Type.Null {
		change |= schema.ChangeNull
	}
	changed, err := d.typeChanged(from, to)
	if err != nil {
		return schema.NoChange, err
	}
	if changed {
		change |= schema.ChangeType
	}
	changed, err = d.defaultChanged(from, to)
	if err != nil {
		return schema.NoChange, err
	}
	if changed {
		change |= schema.ChangeDefault
	}
	return change, nil
}

// IsGeneratedIndexName reports if the index name was generated by the database.
func (d *diff) IsGeneratedIndexName(_ *schema.Table, idx *schema.Index) bool {
	// Auto-generated index names for functional/expression indexes. See.
	// mysql-server/sql/sql_table.cc#add_functional_index_to_create_list
	const f = "functional_index"
	switch {
	case d.supportsIndexExpr() && idx.Name == f:
		return true
	case d.supportsIndexExpr() && strings.HasPrefix(idx.Name+"_", f):
		i, err := strconv.ParseInt(strings.TrimLeft(idx.Name, idx.Name+"_"), 10, 64)
		return err == nil && i > 1
	case len(idx.Parts) == 0 || idx.Parts[0].C == nil:
		return false
	}
	// Unnamed INDEX or UNIQUE constraints are named by
	// the first index-part (as column or part of it).
	// For example, "c", "c_2", "c_3", etc.
	switch name := idx.Parts[0].C.Name; {
	case idx.Name == name:
		return true
	case strings.HasPrefix(idx.Name, name+"_"):
		i, err := strconv.ParseInt(strings.TrimPrefix(idx.Name, name+"_"), 10, 64)
		return err == nil && i > 1
	default:
		return false
	}
}

// IndexAttrChanged reports if the index attributes were changed.
func (*diff) IndexAttrChanged(from, to []schema.Attr) bool {
	return indexType(from).T != indexType(to).T
}

// IndexPartAttrChanged reports if the index-part attributes (collation or prefix) were changed.
func (*diff) IndexPartAttrChanged(from, to *schema.IndexPart) bool {
	var s1, s2 SubPart
	return sqlx.Has(from.Attrs, &s1) != sqlx.Has(to.Attrs, &s2) || s1.Len != s2.Len
}

// ReferenceChanged reports if the foreign key referential action was changed.
func (*diff) ReferenceChanged(from, to schema.ReferenceOption) bool {
	// According to MySQL docs, foreign key constraints are checked
	// immediately, so NO ACTION is the same as RESTRICT. Specifying
	// RESTRICT (or NO ACTION) is the same as omitting the ON DELETE
	// or ON UPDATE clause.
	if from == "" || from == schema.Restrict {
		from = schema.NoAction
	}
	if to == "" || to == schema.Restrict {
		to = schema.NoAction
	}
	return from != to
}

// Normalize implements the sqlx.Normalizer interface.
func (*diff) Normalize(from, to *schema.Table) {
	indexes := make([]*schema.Index, 0, len(from.Indexes))
	for _, idx := range from.Indexes {
		// MySQL requires that foreign key columns be indexed; Therefore, if the child
		// table is defined on non-indexed columns, an index is automatically created
		// to satisfy the constraint.
		// Therefore, if no such key was defined on the desired state, the diff will
		// recommend to drop it on migration. Therefore, we fix it by dropping it from
		// the current state manually.
		if _, ok := to.Index(idx.Name); ok || !keySupportsFK(from, idx) {
			indexes = append(indexes, idx)
		}
	}
	from.Indexes = indexes
}

// collationChange returns the schema change for migrating the collation if
// it was changed and its not the default attribute inherited from its parent.
func (*diff) collationChange(from, top, to []schema.Attr) schema.Change {
	var fromC, topC, toC schema.Collation
	switch fromHas, topHas, toHas := sqlx.Has(from, &fromC), sqlx.Has(top, &topC), sqlx.Has(to, &toC); {
	case !fromHas && !toHas:
	case !fromHas:
		return &schema.AddAttr{
			A: &toC,
		}
	case !toHas:
		// There is no way to DROP a COLLATE that was configured on the table
		// and it is not the default. Therefore, we use ModifyAttr and give it
		// the inherited (and default) collation from schema or server.
		if topHas && fromC.V != topC.V {
			return &schema.ModifyAttr{
				From: &fromC,
				To:   &topC,
			}
		}
	case fromC.V != toC.V:
		return &schema.ModifyAttr{
			From: &fromC,
			To:   &toC,
		}
	}
	return noChange
}

// charsetChange returns the schema change for migrating the collation if
// it was changed and its not the default attribute inherited from its parent.
func (*diff) charsetChange(from, top, to []schema.Attr) schema.Change {
	var fromC, topC, toC schema.Charset
	switch fromHas, topHas, toHas := sqlx.Has(from, &fromC), sqlx.Has(top, &topC), sqlx.Has(to, &toC); {
	case !fromHas && !toHas:
	case !fromHas:
		return &schema.AddAttr{
			A: &toC,
		}
	case !toHas:
		// There is no way to DROP a CHARSET that was configured on the table
		// and it is not the default. Therefore, we use ModifyAttr and give it
		// the inherited (and default) collation from schema or server.
		if topHas && fromC.V != topC.V {
			return &schema.ModifyAttr{
				From: &fromC,
				To:   &topC,
			}
		}
	case fromC.V != toC.V:
		return &schema.ModifyAttr{
			From: &fromC,
			To:   &toC,
		}
	}
	return noChange
}

// autoIncChange returns the schema change for changing the AUTO_INCREMENT
// attribute in case it is not the default.
func (*diff) autoIncChange(from, to []schema.Attr) schema.Change {
	var fromA, toA AutoIncrement
	// The table is empty and AUTO_INCREMENT was not configured. This can happen
	// because older versions of MySQL (< 8.0) stored the AUTO_INCREMENT counter
	// in main memory (not persistent), and the value is reset on process restart.
	if sqlx.Has(from, &fromA) && sqlx.Has(to, &toA) && fromA.V <= 1 && toA.V > 1 {
		return &schema.ModifyAttr{
			From: &fromA,
			To:   &toA,
		}
	}
	return noChange
}

// indexType returns the index type from its attribute.
// The default type is BTREE if no type was specified.
func indexType(attr []schema.Attr) *IndexType {
	t := &IndexType{T: "BTREE"}
	if sqlx.Has(attr, t) {
		t.T = strings.ToUpper(t.T)
	}
	return t
}

// enforced returns the ENFORCED attribute for the CHECK
// constraint. A CHECK is ENFORCED if not state otherwise.
func enforced(attr []schema.Attr) bool {
	if e := (Enforced{}); sqlx.Has(attr, &e) {
		return e.V
	}
	return true
}

// noChange describes a zero change.
var noChange struct{ schema.Change }

func (d *diff) typeChanged(from, to *schema.Column) (bool, error) {
	fromT, toT := from.Type.Type, to.Type.Type
	if fromT == nil || toT == nil {
		return false, fmt.Errorf("mysql: missing type information for column %q", from.Name)
	}
	if reflect.TypeOf(fromT) != reflect.TypeOf(toT) {
		return true, nil
	}
	var changed bool
	switch fromT := fromT.(type) {
	case *schema.BinaryType, *schema.BoolType, *schema.DecimalType, *schema.FloatType:
		changed = mustFormat(fromT) != mustFormat(toT)
	case *schema.EnumType:
		toT := toT.(*schema.EnumType)
		changed = !sqlx.ValuesEqual(fromT.Values, toT.Values)
	case *schema.IntegerType:
		toT := toT.(*schema.IntegerType)
		// MySQL v8.0.19 dropped the display width
		// information from the information schema.
		if d.supportsDisplayWidth() {
			ft, _, _, err := parseColumn(fromT.T)
			if err != nil {
				return false, err
			}
			tt, _, _, err := parseColumn(toT.T)
			if err != nil {
				return false, err
			}
			fromT.T, toT.T = ft[0], tt[0]
		}
		fromW, toW := displayWidth(fromT.Attrs), displayWidth(toT.Attrs)
		changed = fromT.T != toT.T || fromT.Unsigned != toT.Unsigned ||
			(fromW != nil) != (toW != nil) || (fromW != nil && fromW.N != toW.N)
	case *schema.JSONType:
		toT := toT.(*schema.JSONType)
		changed = fromT.T != toT.T
	case *schema.StringType:
		changed = mustFormat(fromT) != mustFormat(toT)
	case *schema.SpatialType:
		toT := toT.(*schema.SpatialType)
		changed = fromT.T != toT.T
	case *schema.TimeType:
		toT := toT.(*schema.TimeType)
		changed = fromT.T != toT.T
	case *BitType:
		toT := toT.(*BitType)
		changed = fromT.T != toT.T
	case *SetType:
		toT := toT.(*SetType)
		changed = !sqlx.ValuesEqual(fromT.Values, toT.Values)
	default:
		return false, &sqlx.UnsupportedTypeError{Type: fromT}
	}
	return changed, nil
}

// defaultChanged reports if the a default value of a column
// type was changed.
func (d *diff) defaultChanged(from, to *schema.Column) (bool, error) {
	d1, ok1 := sqlx.DefaultValue(from)
	d2, ok2 := sqlx.DefaultValue(to)
	if ok1 != ok2 {
		return true, nil
	}
	if d1 == d2 {
		return false, nil
	}
	switch from.Type.Type.(type) {
	case *schema.BinaryType:
		a, err1 := binValue(d1)
		b, err2 := binValue(d2)
		if err1 != nil || err2 != nil {
			return true, nil
		}
		return !equalsStringValues(a, b), nil
	case *schema.BoolType:
		a, err1 := boolValue(d1)
		b, err2 := boolValue(d2)
		if err1 == nil && err2 == nil {
			return a != b, nil
		}
		return false, nil
	case *schema.IntegerType:
		return !d.equalsIntValues(d1, d2), nil
	case *schema.EnumType, *SetType, *schema.StringType:
		return !equalsStringValues(d1, d2), nil
	case *schema.TimeType:
		x1 := strings.ToLower(strings.Trim(d1, "' ()"))
		x2 := strings.ToLower(strings.Trim(d2, "' ()"))
		return x1 != x2, nil
	default:
		x1 := strings.Trim(d1, "'")
		x2 := strings.Trim(d2, "'")
		return x1 != x2, nil
	}
}

// equalsIntValues report if the 2 int default values are ~equal.
// Note that default expression are not supported atm.
func (d *diff) equalsIntValues(x1, x2 string) bool {
	x1 = strings.ToLower(strings.Trim(x1, "' "))
	x2 = strings.ToLower(strings.Trim(x2, "' "))
	if x1 == x2 {
		return true
	}
	d1, err := strconv.ParseInt(x1, 10, 64)
	if err != nil {
		// Numbers are rounded down to their nearest integer.
		f, err := strconv.ParseFloat(x1, 64)
		if err != nil {
			return false
		}
		d1 = int64(f)
	}
	d2, err := strconv.ParseInt(x2, 10, 64)
	if err != nil {
		// Numbers are rounded down to their nearest integer.
		f, err := strconv.ParseFloat(x2, 64)
		if err != nil {
			return false
		}
		d2 = int64(f)
	}
	return d1 == d2
}

// equalsStringValues report if the 2 string default values are
// equal after dropping their quotes.
func equalsStringValues(x1, x2 string) bool {
	a, err1 := sqlx.Unquote(x1)
	b, err2 := sqlx.Unquote(x2)
	return a == b && err1 == nil && err2 == nil
}

// boolValue returns the MySQL boolean value from the given string (if it is known).
func boolValue(x string) (bool, error) {
	switch x {
	case "1", "'1'", "TRUE", "true":
		return true, nil
	case "0", "'0'", "FALSE", "false":
		return false, nil
	default:
		return false, fmt.Errorf("mysql: unknown value: %q", x)
	}
}

// binValue returns the MySQL binary value from the given string (if it is known).
func binValue(x string) (string, error) {
	if !isHex(x) {
		return x, nil
	}
	d, err := hex.DecodeString(x[2:])
	if err != nil {
		return x, err
	}
	return string(d), nil
}

func displayWidth(attr []schema.Attr) *DisplayWidth {
	var (
		z *ZeroFill
		d *DisplayWidth
	)
	for i := range attr {
		switch at := attr[i].(type) {
		case *ZeroFill:
			z = at
		case *DisplayWidth:
			d = at
		}
	}
	// Accept the display width only if
	// the zerofill attribute is defined.
	if z == nil || d == nil {
		return nil
	}
	return d
}

// keySupportsFK reports if the index key was created automatically by MySQL
// to support the constraint. See sql/sql_table.cc#find_fk_supporting_key.
func keySupportsFK(t *schema.Table, idx *schema.Index) bool {
	if _, ok := t.ForeignKey(idx.Name); ok {
		return true
	}
search:
	for _, fk := range t.ForeignKeys {
		if len(fk.Columns) != len(idx.Parts) {
			continue
		}
		for i, c := range fk.Columns {
			if idx.Parts[i].C == nil || idx.Parts[i].C.Name != c.Name {
				continue search
			}
		}
		return true
	}
	return false
}
