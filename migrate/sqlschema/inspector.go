package sqlschema

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/schema"
	orderedmap "github.com/wk8/go-ordered-map/v2"
)

type InspectorDialect interface {
	schema.Dialect
	Inspector(db *bun.DB, excludeTables ...string) Inspector

	// EquivalentType returns true if col1 and co2 SQL types are equivalent,
	// i.e. they might use dialect-specifc type aliases (SERIAL ~ SMALLINT)
	// or specify the same VARCHAR length differently (VARCHAR(255) ~ VARCHAR).
	EquivalentType(Column, Column) bool
}

// Inspector reads schema state.
type Inspector interface {
	Inspect(ctx context.Context) (Database, error)
}

// inspector is opaque pointer to a databse inspector.
type inspector struct {
	Inspector
}

// NewInspector creates a new database inspector, if the dialect supports it.
func NewInspector(db *bun.DB, excludeTables ...string) (Inspector, error) {
	dialect, ok := (db.Dialect()).(InspectorDialect)
	if !ok {
		return nil, fmt.Errorf("%s does not implement sqlschema.Inspector", db.Dialect().Name())
	}
	return &inspector{
		Inspector: dialect.Inspector(db, excludeTables...),
	}, nil
}

// BunModelInspector creates the current project state from the passed bun.Models.
// Do not recycle BunModelInspector for different sets of models, as older models will not be de-registerred before the next run.
type BunModelInspector struct {
	tables *schema.Tables
}

var _ Inspector = (*BunModelInspector)(nil)

func NewBunModelInspector(tables *schema.Tables) *BunModelInspector {
	return &BunModelInspector{
		tables: tables,
	}
}

// BunModelSchema is the schema state derived from bun table models.
type BunModelSchema struct {
	BaseDatabase

	Tables *orderedmap.OrderedMap[string, Table]
}

func (ms BunModelSchema) GetTables() *orderedmap.OrderedMap[string, Table] {
	return ms.Tables
}

// BunTable provides additional table metadata that is only accessible from scanning bun models.
type BunTable struct {
	BaseTable

	// Model stores the zero interface to the underlying Go struct.
	Model interface{}
}

func (bmi *BunModelInspector) Inspect(ctx context.Context) (Database, error) {
	state := BunModelSchema{
		BaseDatabase: BaseDatabase{
			ForeignKeys: make(map[ForeignKey]string),
		},
		Tables: orderedmap.New[string, Table](),
	}
	for _, t := range bmi.tables.All() {
		columns := orderedmap.New[string, Column]()
		for _, f := range t.Fields {

			sqlType, length, err := parseLen(f.CreateTableSQLType)
			if err != nil {
				return nil, fmt.Errorf("parse length in %q: %w", f.CreateTableSQLType, err)
			}
			columns.Set(f.Name, &BaseColumn{
				Name:            f.Name,
				SQLType:         strings.ToLower(sqlType), // TODO(dyma): maybe this is not necessary after Column.Eq()
				VarcharLen:      length,
				DefaultValue:    exprToLower(f.SQLDefault),
				IsNullable:      !f.NotNull,
				IsAutoIncrement: f.AutoIncrement,
				IsIdentity:      f.Identity,
			})
		}

		var unique []Unique
		for name, group := range t.Unique {
			// Create a separate unique index for single-column unique constraints
			//  let each dialect apply the default naming convention.
			if name == "" {
				for _, f := range group {
					unique = append(unique, Unique{Columns: NewColumns(f.Name)})
				}
				continue
			}

			// Set the name if it is a "unique group", in which case the user has provided the name.
			var columns []string
			for _, f := range group {
				columns = append(columns, f.Name)
			}
			unique = append(unique, Unique{Name: name, Columns: NewColumns(columns...)})
		}

		var pk *PrimaryKey
		if len(t.PKs) > 0 {
			var columns []string
			for _, f := range t.PKs {
				columns = append(columns, f.Name)
			}
			pk = &PrimaryKey{Columns: NewColumns(columns...)}
		}

		state.Tables.Set(t.Name, &BunTable{
			BaseTable: BaseTable{
				Schema:            t.Schema,
				Name:              t.Name,
				Columns:           columns,
				UniqueConstraints: unique,
				PrimaryKey:        pk,
			},
			Model: t.ZeroIface,
		})

		for _, rel := range t.Relations {
			// These relations are nominal and do not need a foreign key to be declared in the current table.
			// They will be either expressed as N:1 relations in an m2m mapping table, or will be referenced by the other table if it's a 1:N.
			if rel.Type == schema.ManyToManyRelation ||
				rel.Type == schema.HasManyRelation {
				continue
			}

			var fromCols, toCols []string
			for _, f := range rel.BasePKs {
				fromCols = append(fromCols, f.Name)
			}
			for _, f := range rel.JoinPKs {
				toCols = append(toCols, f.Name)
			}

			target := rel.JoinTable
			state.ForeignKeys[ForeignKey{
				From: NewColumnReference(t.Schema, t.Name, fromCols...),
				To:   NewColumnReference(target.Schema, target.Name, toCols...),
			}] = ""
		}
	}
	return state, nil
}

func parseLen(typ string) (string, int, error) {
	paren := strings.Index(typ, "(")
	if paren == -1 {
		return typ, 0, nil
	}
	length, err := strconv.Atoi(typ[paren+1 : len(typ)-1])
	if err != nil {
		return typ, 0, err
	}
	return typ[:paren], length, nil
}

// exprToLower converts string to lowercase, if it does not contain a string literal 'lit'.
// Use it to ensure that user-defined default values in the models are always comparable
// to those returned by the database inspector, regardless of the case convention in individual drivers.
func exprToLower(s string) string {
	if strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'") {
		return s
	}
	return strings.ToLower(s)
}
