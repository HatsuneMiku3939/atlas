package mysql

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"ariga.io/atlas/schema/schemaspec"
	"ariga.io/atlas/schema/schemaspec/schemahcl"
	"ariga.io/atlas/sql/internal/specutil"
	"ariga.io/atlas/sql/internal/sqlx"
	"ariga.io/atlas/sql/schema"
	"ariga.io/atlas/sql/sqlspec"
)

type doc struct {
	Tables  []*sqlspec.Table  `spec:"table"`
	Schemas []*sqlspec.Schema `spec:"schema"`
}

// evalSpec unmarshals an Atlas DDL document using an unmarshaler into v.
func evalSpec(data []byte, v interface{}, input map[string]string) error {
	var d doc
	if err := hclState.Eval(data, &d, input); err != nil {
		return err
	}
	switch v := v.(type) {
	case *schema.Realm:
		err := specutil.Scan(v, d.Schemas, d.Tables, convertTable)
		if err != nil {
			return fmt.Errorf("mysql: failed converting to *schema.Realm: %w", err)
		}
		for _, schemaSpec := range d.Schemas {
			schm, ok := v.Schema(schemaSpec.Name)
			if !ok {
				return fmt.Errorf("could not find schema: %q", schemaSpec.Name)
			}
			if err := convertCharset(schemaSpec, &schm.Attrs); err != nil {
				return err
			}
		}
	case *schema.Schema:
		if len(d.Schemas) != 1 {
			return fmt.Errorf("mysql: expecting document to contain a single schema, got %d", len(d.Schemas))
		}
		var r schema.Realm
		if err := specutil.Scan(&r, d.Schemas, d.Tables, convertTable); err != nil {
			return err
		}
		if err := convertCharset(d.Schemas[0], &r.Schemas[0].Attrs); err != nil {
			return err
		}
		r.Schemas[0].Realm = nil
		*v = *r.Schemas[0]
	default:
		return fmt.Errorf("mysql: failed unmarshaling spec. %T is not supported", v)
	}
	return nil
}

// MarshalSpec marshals v into an Atlas DDL document using a schemaspec.Marshaler.
func MarshalSpec(v interface{}, marshaler schemaspec.Marshaler) ([]byte, error) {
	return specutil.Marshal(v, marshaler, schemaSpec)
}

var (
	hclState = schemahcl.New(
		schemahcl.WithTypes(TypeRegistry.Specs()),
		schemahcl.WithScopedEnums("table.index.type", IndexTypeBTree, IndexTypeHash, IndexTypeFullText, IndexTypeSpatial),
		schemahcl.WithScopedEnums("table.column.as.type", stored, persistent, virtual),
		schemahcl.WithScopedEnums("table.foreign_key.on_update", specutil.ReferenceVars...),
		schemahcl.WithScopedEnums("table.foreign_key.on_delete", specutil.ReferenceVars...),
	)
	// UnmarshalHCL unmarshals an Atlas HCL DDL document into v.
	UnmarshalHCL = schemaspec.UnmarshalerFunc(func(data []byte, v interface{}) error {
		return evalSpec(data, v, nil)
	})
	// MarshalHCL marshals v into an Atlas HCL DDL document.
	MarshalHCL = schemaspec.MarshalerFunc(func(v interface{}) ([]byte, error) {
		return MarshalSpec(v, hclState)
	})
	// EvalHCL implements the schemahcl.Evaluator interface.
	EvalHCL = schemahcl.EvalFunc(evalSpec)
)

// convertTable converts a sqlspec.Table to a schema.Table. Table conversion is done without converting
// ForeignKeySpecs into ForeignKeys, as the target tables do not necessarily exist in the schema
// at this point. Instead, the linking is done by the convertSchema function.
func convertTable(spec *sqlspec.Table, parent *schema.Schema) (*schema.Table, error) {
	t, err := specutil.Table(spec, parent, convertColumn, specutil.PrimaryKey, convertIndex, convertCheck)
	if err != nil {
		return nil, err
	}
	if err := convertCharset(spec, &t.Attrs); err != nil {
		return nil, err
	}
	// MySQL allows setting the initial AUTO_INCREMENT value
	// on the table definition.
	if attr, ok := spec.Attr("auto_increment"); ok {
		v, err := attr.Int64()
		if err != nil {
			return nil, err
		}
		t.AddAttrs(&AutoIncrement{V: v})
	}
	return t, err
}

// convertIndex converts a sqlspec.Index into a schema.Index.
func convertIndex(spec *sqlspec.Index, parent *schema.Table) (*schema.Index, error) {
	idx, err := specutil.Index(spec, parent, convertPart)
	if err != nil {
		return nil, err
	}
	if attr, ok := spec.Attr("type"); ok {
		t, err := attr.String()
		if err != nil {
			return nil, err
		}
		idx.AddAttrs(&IndexType{T: t})
	}
	return idx, nil
}

func convertPart(spec *sqlspec.IndexPart, part *schema.IndexPart) error {
	if attr, ok := spec.Attr("prefix"); ok {
		if part.X != nil {
			return errors.New("attribute 'on.prefix' cannot be used in functional part")
		}
		p, err := attr.Int()
		if err != nil {
			return err
		}
		part.AddAttrs(&SubPart{Len: p})
	}
	return nil
}

// convertCheck converts a sqlspec.Check into a schema.Check.
func convertCheck(spec *sqlspec.Check) (*schema.Check, error) {
	c, err := specutil.Check(spec)
	if err != nil {
		return nil, err
	}
	if attr, ok := spec.Attr("enforced"); ok {
		b, err := attr.Bool()
		if err != nil {
			return nil, err
		}
		c.AddAttrs(&Enforced{V: b})
	}
	return c, nil
}

// convertColumn converts a sqlspec.Column into a schema.Column.
func convertColumn(spec *sqlspec.Column, _ *schema.Table) (*schema.Column, error) {
	c, err := specutil.Column(spec, convertColumnType)
	if err != nil {
		return nil, err
	}
	if err := convertCharset(spec, &c.Attrs); err != nil {
		return nil, err
	}
	if attr, ok := spec.Attr("on_update"); ok {
		exp, ok := attr.V.(*schemaspec.RawExpr)
		if !ok {
			return nil, fmt.Errorf(`unexpected type %T for atrribute "on_update"`, attr.V)
		}
		c.AddAttrs(&OnUpdate{A: exp.X})
	}
	if attr, ok := spec.Attr("auto_increment"); ok {
		b, err := attr.Bool()
		if err != nil {
			return nil, err
		}
		if b {
			c.AddAttrs(&AutoIncrement{})
		}
	}
	if err := specutil.ConvertGenExpr(spec.Remain(), c, storedOrVirtual); err != nil {
		return nil, err
	}
	return c, err
}

// convertColumnType converts a sqlspec.Column into a concrete MySQL schema.Type.
func convertColumnType(spec *sqlspec.Column) (schema.Type, error) {
	return TypeRegistry.Type(spec.Type, spec.Extra.Attrs)
}

// schemaSpec converts from a concrete MySQL schema to Atlas specification.
func schemaSpec(s *schema.Schema) (*sqlspec.Schema, []*sqlspec.Table, error) {
	sc, t, err := specutil.FromSchema(s, tableSpec)
	if err != nil {
		return nil, nil, err
	}
	if c, ok := hasCharset(s.Attrs, nil); ok {
		sc.Extra.Attrs = append(sc.Extra.Attrs, specutil.StrAttr("charset", c))
	}
	if c, ok := hasCollate(s.Attrs, nil); ok {
		sc.Extra.Attrs = append(sc.Extra.Attrs, specutil.StrAttr("collate", c))
	}
	return sc, t, nil
}

// tableSpec converts from a concrete MySQL sqlspec.Table to a schema.Table.
func tableSpec(t *schema.Table) (*sqlspec.Table, error) {
	ts, err := specutil.FromTable(
		t,
		columnSpec,
		specutil.FromPrimaryKey,
		indexSpec,
		specutil.FromForeignKey,
		checkSpec,
	)
	if err != nil {
		return nil, err
	}
	if c, ok := hasCharset(t.Attrs, t.Schema.Attrs); ok {
		ts.Extra.Attrs = append(ts.Extra.Attrs, specutil.StrAttr("charset", c))
	}
	if c, ok := hasCollate(t.Attrs, t.Schema.Attrs); ok {
		ts.Extra.Attrs = append(ts.Extra.Attrs, specutil.StrAttr("collate", c))
	}
	return ts, nil
}

func indexSpec(idx *schema.Index) (*sqlspec.Index, error) {
	spec, err := specutil.FromIndex(idx, partAttr)
	if err != nil {
		return nil, err
	}
	// Avoid printing the index type if it is the default.
	if i := (IndexType{}); sqlx.Has(idx.Attrs, &i) && i.T != IndexTypeBTree {
		spec.Extra.Attrs = append(spec.Extra.Attrs, specutil.VarAttr("type", strings.ToUpper(i.T)))
	}
	return spec, nil
}

func partAttr(part *schema.IndexPart, spec *sqlspec.IndexPart) {
	if p := (SubPart{}); sqlx.Has(part.Attrs, &p) && p.Len > 0 {
		spec.Extra.Attrs = append(spec.Extra.Attrs, specutil.IntAttr("prefix", p.Len))
	}
}

// columnSpec converts from a concrete MySQL schema.Column into a sqlspec.Column.
func columnSpec(c *schema.Column, t *schema.Table) (*sqlspec.Column, error) {
	spec, err := specutil.FromColumn(c, columnTypeSpec)
	if err != nil {
		return nil, err
	}
	if c, ok := hasCharset(c.Attrs, t.Attrs); ok {
		spec.Extra.Attrs = append(spec.Extra.Attrs, specutil.StrAttr("charset", c))
	}
	if c, ok := hasCollate(c.Attrs, t.Attrs); ok {
		spec.Extra.Attrs = append(spec.Extra.Attrs, specutil.StrAttr("collate", c))
	}
	if o := (OnUpdate{}); sqlx.Has(c.Attrs, &o) {
		spec.Extra.Attrs = append(spec.Extra.Attrs, specutil.RawAttr("on_update", o.A))
	}
	if sqlx.Has(c.Attrs, &AutoIncrement{}) {
		spec.Extra.Attrs = append(spec.Extra.Attrs, specutil.BoolAttr("auto_increment", true))
	}
	if x := (schema.GeneratedExpr{}); sqlx.Has(c.Attrs, &x) {
		spec.Extra.Children = append(spec.Extra.Children, specutil.FromGenExpr(x, storedOrVirtual))
	}
	return spec, nil
}

// storedOrVirtual returns a STORED or VIRTUAL
// generated type option based on the given string.
func storedOrVirtual(s string) string {
	switch s = strings.ToUpper(s); s {
	// The default is VIRTUAL if no type is specified.
	case "":
		return virtual
	// In MariaDB, PERSISTENT is synonyms for STORED.
	case persistent:
		return stored
	}
	return s
}

// checkSpec converts from a concrete MySQL schema.Check into a sqlspec.Check.
func checkSpec(s *schema.Check) *sqlspec.Check {
	c := specutil.FromCheck(s)
	if e := (Enforced{}); sqlx.Has(s.Attrs, &e) {
		c.Extra.Attrs = append(c.Extra.Attrs, specutil.BoolAttr("enforced", true))
	}
	return c
}

// columnTypeSpec converts from a concrete MySQL schema.Type into sqlspec.Column Type.
func columnTypeSpec(t schema.Type) (*sqlspec.Column, error) {
	st, err := TypeRegistry.Convert(t)
	if err != nil {
		return nil, err
	}
	c := &sqlspec.Column{Type: st}
	for _, attr := range st.Attrs {
		// TODO(rotemtam): infer this from the TypeSpec
		if attr.K == "unsigned" {
			c.Extra.Attrs = append(c.Extra.Attrs, attr)
		}
	}
	return c, nil
}

// convertCharset converts spec charset/collation
// attributes to schema element attributes.
func convertCharset(spec specutil.Attrer, attrs *[]schema.Attr) error {
	if attr, ok := spec.Attr("charset"); ok {
		s, err := attr.String()
		if err != nil {
			return err
		}
		*attrs = append(*attrs, &schema.Charset{V: s})
	}
	// For backwards compatibility, accepts both "collate" and "collation".
	attr, ok := spec.Attr("collate")
	if !ok {
		attr, ok = spec.Attr("collation")
	}
	if ok {
		s, err := attr.String()
		if err != nil {
			return err
		}
		*attrs = append(*attrs, &schema.Collation{V: s})
	}
	return nil
}

// hasCharset reports if the attribute contains the "charset" attribute,
// and it needs to be defined explicitly on the schema. This is true, in
// case the element charset is different from its parent charset.
func hasCharset(attr []schema.Attr, parent []schema.Attr) (string, bool) {
	var c, p schema.Charset
	if sqlx.Has(attr, &c) && (parent == nil || sqlx.Has(parent, &p) && c.V != p.V) {
		return c.V, true
	}
	return "", false
}

// hasCollate reports if the attribute contains the "collation"/"collate" attribute,
// and it needs to be defined explicitly on the schema. This is true, in
// case the element collation is different from its parent collation.
func hasCollate(attr []schema.Attr, parent []schema.Attr) (string, bool) {
	var c, p schema.Collation
	if sqlx.Has(attr, &c) && (parent == nil || sqlx.Has(parent, &p) && c.V != p.V) {
		return c.V, true
	}
	return "", false
}

// TypeRegistry contains the supported TypeSpecs for the mysql driver.
var TypeRegistry = schemahcl.NewRegistry(
	schemahcl.WithFormatter(FormatType),
	schemahcl.WithParser(ParseType),
	schemahcl.WithSpecs(
		&schemaspec.TypeSpec{
			Name: TypeEnum,
			T:    TypeEnum,
			Attributes: []*schemaspec.TypeAttr{
				{Name: "values", Kind: reflect.Slice, Required: true},
			},
			RType: reflect.TypeOf(schema.EnumType{}),
		},
		&schemaspec.TypeSpec{
			Name: TypeSet,
			T:    TypeSet,
			Attributes: []*schemaspec.TypeAttr{
				{Name: "values", Kind: reflect.Slice, Required: true},
			},
			RType: reflect.TypeOf(SetType{}),
		},
		schemahcl.TypeSpec(TypeBool),
		schemahcl.TypeSpec(TypeBoolean),
		schemahcl.TypeSpec(TypeBit, schemahcl.WithAttributes(schemahcl.SizeTypeAttr(false))),
		schemahcl.TypeSpec(TypeInt, schemahcl.WithAttributes(unsignedTypeAttr(), schemahcl.SizeTypeAttr(false))),
		schemahcl.TypeSpec(TypeTinyInt, schemahcl.WithAttributes(unsignedTypeAttr(), schemahcl.SizeTypeAttr(false))),
		schemahcl.TypeSpec(TypeSmallInt, schemahcl.WithAttributes(unsignedTypeAttr(), schemahcl.SizeTypeAttr(false))),
		schemahcl.TypeSpec(TypeMediumInt, schemahcl.WithAttributes(unsignedTypeAttr(), schemahcl.SizeTypeAttr(false))),
		schemahcl.TypeSpec(TypeBigInt, schemahcl.WithAttributes(unsignedTypeAttr(), schemahcl.SizeTypeAttr(false))),
		schemahcl.TypeSpec(TypeDecimal, schemahcl.WithAttributes(unsignedTypeAttr(), &schemaspec.TypeAttr{Name: "precision", Kind: reflect.Int, Required: false}, &schemaspec.TypeAttr{Name: "scale", Kind: reflect.Int, Required: false})),
		schemahcl.TypeSpec(TypeNumeric, schemahcl.WithAttributes(unsignedTypeAttr(), &schemaspec.TypeAttr{Name: "precision", Kind: reflect.Int, Required: false}, &schemaspec.TypeAttr{Name: "scale", Kind: reflect.Int, Required: false})),
		schemahcl.TypeSpec(TypeFloat, schemahcl.WithAttributes(unsignedTypeAttr(), &schemaspec.TypeAttr{Name: "precision", Kind: reflect.Int, Required: false}, &schemaspec.TypeAttr{Name: "scale", Kind: reflect.Int, Required: false})),
		schemahcl.TypeSpec(TypeDouble, schemahcl.WithAttributes(unsignedTypeAttr(), &schemaspec.TypeAttr{Name: "precision", Kind: reflect.Int, Required: false}, &schemaspec.TypeAttr{Name: "scale", Kind: reflect.Int, Required: false})),
		schemahcl.TypeSpec(TypeReal, schemahcl.WithAttributes(unsignedTypeAttr(), &schemaspec.TypeAttr{Name: "precision", Kind: reflect.Int, Required: false}, &schemaspec.TypeAttr{Name: "scale", Kind: reflect.Int, Required: false})),
		schemahcl.TypeSpec(TypeTimestamp, schemahcl.WithAttributes(&schemaspec.TypeAttr{Name: "precision", Kind: reflect.Int, Required: false})),
		schemahcl.TypeSpec(TypeDate, schemahcl.WithAttributes(&schemaspec.TypeAttr{Name: "precision", Kind: reflect.Int, Required: false})),
		schemahcl.TypeSpec(TypeTime, schemahcl.WithAttributes(&schemaspec.TypeAttr{Name: "precision", Kind: reflect.Int, Required: false})),
		schemahcl.TypeSpec(TypeDateTime, schemahcl.WithAttributes(&schemaspec.TypeAttr{Name: "precision", Kind: reflect.Int, Required: false})),
		schemahcl.TypeSpec(TypeYear, schemahcl.WithAttributes(&schemaspec.TypeAttr{Name: "precision", Kind: reflect.Int, Required: false})),
		schemahcl.TypeSpec(TypeVarchar, schemahcl.WithAttributes(schemahcl.SizeTypeAttr(true))),
		schemahcl.TypeSpec(TypeChar, schemahcl.WithAttributes(schemahcl.SizeTypeAttr(false))),
		schemahcl.TypeSpec(TypeVarBinary, schemahcl.WithAttributes(schemahcl.SizeTypeAttr(false))),
		schemahcl.TypeSpec(TypeBinary, schemahcl.WithAttributes(schemahcl.SizeTypeAttr(false))),
		schemahcl.TypeSpec(TypeBlob, schemahcl.WithAttributes(schemahcl.SizeTypeAttr(false))),
		schemahcl.TypeSpec(TypeTinyBlob),
		schemahcl.TypeSpec(TypeMediumBlob),
		schemahcl.TypeSpec(TypeLongBlob),
		schemahcl.TypeSpec(TypeJSON),
		schemahcl.TypeSpec(TypeText, schemahcl.WithAttributes(schemahcl.SizeTypeAttr(false))),
		schemahcl.TypeSpec(TypeTinyText),
		schemahcl.TypeSpec(TypeMediumText),
		schemahcl.TypeSpec(TypeLongText),
		schemahcl.TypeSpec(TypeGeometry),
		schemahcl.TypeSpec(TypePoint),
		schemahcl.TypeSpec(TypeMultiPoint),
		schemahcl.TypeSpec(TypeLineString),
		schemahcl.TypeSpec(TypeMultiLineString),
		schemahcl.TypeSpec(TypePolygon),
		schemahcl.TypeSpec(TypeMultiPolygon),
		schemahcl.TypeSpec(TypeGeometryCollection),
	),
)

func unsignedTypeAttr() *schemaspec.TypeAttr {
	return &schemaspec.TypeAttr{
		Name: "unsigned",
		Kind: reflect.Bool,
	}
}
