package gorm

import (
	"runtime/debug"
	"database/sql"
	"fmt"
	"go/ast"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"sync"
)

var modelStructs_cacheMutex  = &sync.Mutex{}
var modelStructs_byScopeType = map[reflect.Type]*ModelStruct{}
var modelStructs_byTableName = map[string      ]*ModelStruct{}
var modelStruct_last *ModelStruct

type ModelStruct struct {
	PrimaryFields []*StructField
	StructFields  []*StructField
	ModelType     reflect.Type
	TableName     func(*DB) string
}

type StructField struct {
	DBName          string
	Name            string
	Names           []string
	IsPrimaryKey    bool
	IsNormal        bool
	IsIgnored       bool
	IsScanner       bool
	HasDefaultValue bool
	Tag             reflect.StructTag
	Struct          reflect.StructField
	IsForeignKey    bool
	IgnoreUniqueIndex bool
	IgnorePrimaryKey bool
	Relationship    *Relationship
	Value		reflect.Value
}

func (structField *StructField) clone() *StructField {
	return &StructField{
		DBName:          structField.DBName,
		Name:            structField.Name,
		Names:           structField.Names,
		IsPrimaryKey:    structField.IsPrimaryKey,
		IsNormal:        structField.IsNormal,
		IsIgnored:       structField.IsIgnored,
		IsScanner:       structField.IsScanner,
		HasDefaultValue: structField.HasDefaultValue,
		Tag:             structField.Tag,
		Struct:          structField.Struct,
		IsForeignKey:    structField.IsForeignKey,
		Relationship:    structField.Relationship,
		Value:           structField.Value,
	}
}

type Relationship struct {
	Kind                        string
	PolymorphicType             string
	PolymorphicDBName           string
	ForeignFieldName            string
	ForeignDBName               string
	AssociationForeignFieldName string
	AssociationForeignDBName    string
	JoinTableHandler            JoinTableHandlerInterface
}

var pluralMapKeys = []*regexp.Regexp{regexp.MustCompile("ch$"), regexp.MustCompile("ss$"), regexp.MustCompile("sh$"), regexp.MustCompile("day$"), regexp.MustCompile("y$"), regexp.MustCompile("x$"), regexp.MustCompile("([^s])s?$")}
var pluralMapValues = []string{"ches", "sses", "shes", "days", "ies", "xes", "${1}s"}

func (scope *Scope) GetModelStruct() *ModelStruct {
	var tableName string
	var modelStruct ModelStruct

	reflectValue := reflect.Indirect(reflect.ValueOf(scope.Value))
	if !reflectValue.IsValid() {
		return &modelStruct
	}

	if reflectValue.Kind() == reflect.Slice {
		reflectValue = reflect.Indirect(reflect.New(reflectValue.Type().Elem()))
	}

	scopeType := reflectValue.Type()

	if scopeType.Kind() == reflect.Ptr {
		scopeType = scopeType.Elem()
	}

	{
		modelStructs_cacheMutex.Lock();
		value, ok := modelStructs_byScopeType[scopeType]
		modelStructs_cacheMutex.Unlock();
		if ok {
			return value
		}
	}

	modelStruct.ModelType = scopeType
	if scopeType.Kind() != reflect.Struct {
		modelStructs_cacheMutex.Lock()
		modelStruct_last = &modelStruct
		modelStructs_cacheMutex.Unlock()
		return &modelStruct
	}

	// Getting table name appendix
	for i := 0; i < scopeType.NumField(); i++ {
		if fieldStruct := scopeType.Field(i); ast.IsExported(fieldStruct.Name) {
			if (fieldStruct.Type.Kind() == reflect.Interface) {
				if (fieldStruct.Tag.Get("sql") == "-") {
					continue;
				}
				// Interface field
				value := reflect.ValueOf(scope.Value).Elem()
				if (value.Kind() == reflect.Slice) {
					// A slice, using the first element
					value = value.Index(0)
				}
				value  = reflect.ValueOf(value.Field(i).Interface())
				if (! value.IsValid()) { // Invalid interfaces, using Model()'s result
					modelStructs_cacheMutex.Lock()
					defer func() {modelStructs_cacheMutex.Unlock()}()
					if (modelStruct_last == nil) { // It's nil? That's bad
						fmt.Printf("modelStruct_last == nil\n");
						debug.PrintStack();
						panic(nil);
						return &ModelStruct{};
					}
					return modelStruct_last
				}
				tableName = tableName + "__" + value.Elem().Type().Name()
			}
		}
	}
	// Set tablename
	if fm := reflect.New(scopeType).MethodByName("TableName"); fm.IsValid() {
		if results := fm.Call([]reflect.Value{}); len(results) > 0 {
			if name, ok := results[0].Interface().(string); ok {
				tableName = name + tableName
				modelStruct.TableName = func(*DB) string {
					return tableName
				}
			}
		}
	} else {
		name := ToDBName(scopeType.Name())
		if scope.db == nil || !scope.db.parent.singularTable {
			for index, reg := range pluralMapKeys {
				if reg.MatchString(name) {
					name = reg.ReplaceAllString(name, pluralMapValues[index])
				}
			}
		}

		tableName = name + tableName
		modelStruct.TableName = func(*DB) string {
			return tableName
		}
	}

	{
		modelStructs_cacheMutex.Lock();
		value, ok := modelStructs_byTableName[tableName]
		modelStructs_cacheMutex.Unlock();
		if ok {
			modelStructs_cacheMutex.Lock();
			defer func() {modelStructs_cacheMutex.Unlock()}();
			modelStruct_last = value
			if (value == nil) { // It's nil? That's bad
				fmt.Printf("value == nil\n")
				debug.PrintStack()
				panic(nil)
				return &ModelStruct{}
			}
			return value
		}
	}

	// Get all fields
	cachable_byScopeType := true
	fields := []*StructField{}
	for i := 0; i < scopeType.NumField(); i++ {
		if fieldStruct := scopeType.Field(i); ast.IsExported(fieldStruct.Name) {
			var value reflect.Value
			if (fieldStruct.Type.Kind() == reflect.Interface) {
				if (fieldStruct.Tag.Get("sql") == "-") {
					continue;
				}
				value = reflect.ValueOf(scope.Value).Elem()
				if (value.Kind() == reflect.Slice) {
					value = value.Index(0)
				}
				value = reflect.ValueOf(value.Field(i).Interface())
				cachable_byScopeType = false
			} else {
				value = reflect.Indirect(reflect.ValueOf(scope.Value))
			}
			if (value.Kind() == reflect.Slice) {
				if (value.Len() == 0) {
					value = reflect.MakeSlice(value.Type(), 1, 1);
				}
				value = value.Index(0);
			}
			field := &StructField{
				Struct: fieldStruct,
				Value:  value,
				Name:   fieldStruct.Name,
				Names:  []string{fieldStruct.Name},
				Tag:    fieldStruct.Tag,
			}

			if (fieldStruct.Tag.Get("sql") == "-") {
				field.IsIgnored = true
			} else {
				sqlSettings := parseTagSetting(field.Tag.Get("sql"))
				gormSettings := parseTagSetting(field.Tag.Get("gorm"))
				if _, ok := gormSettings["PRIMARY_KEY"]; ok {
					field.IsPrimaryKey = true
					modelStruct.PrimaryFields = append(modelStruct.PrimaryFields, field)
				}

				if _, ok := sqlSettings["DEFAULT"]; ok {
					field.HasDefaultValue = true
				}

				if value, ok := gormSettings["COLUMN"]; ok {
					field.DBName = value
				} else {
					field.DBName = ToDBName(fieldStruct.Name)
				}
			}
			fields = append(fields, field)
		}
	}

	defer func() {
		for _, field := range fields {
			if !field.IsIgnored {
				fieldStruct := field.Struct
				fieldType, indirectType := fieldStruct.Type, fieldStruct.Type
				if indirectType.Kind() == reflect.Ptr {
					indirectType = indirectType.Elem()
				}

				if _, isScanner := reflect.New(fieldType).Interface().(sql.Scanner); isScanner {
					field.IsScanner, field.IsNormal = true, true
				}

				if _, isTime := reflect.New(indirectType).Interface().(*time.Time); isTime {
					field.IsNormal = true
				}

				if !field.IsNormal {
					var iface interface{}
					gormSettings := parseTagSetting(field.Tag.Get("gorm"))
					if (fieldStruct.Type.Kind() == reflect.Interface) {
						indirectType = (*field).Value.Elem().Type()
						iface = (*field).Value.Elem().Interface()
					} else {
						iface = reflect.New(fieldStruct.Type).Interface()
					}
					toScope := scope.New(iface)

					getForeignField := func(column string, fields []*StructField) *StructField {
						for _, field := range fields {
							if field.Name == column || field.DBName == ToDBName(column) {
								return field
							}
						}
						return nil
					}

					var relationship = &Relationship{}

					foreignKey := gormSettings["FOREIGNKEY"]
					if polymorphic := gormSettings["POLYMORPHIC"]; polymorphic != "" {
						if polymorphicField := getForeignField(polymorphic+"Id", toScope.GetStructFields()); polymorphicField != nil {
							if polymorphicType := getForeignField(polymorphic+"Type", toScope.GetStructFields()); polymorphicType != nil {
								relationship.ForeignFieldName = polymorphicField.Name
								relationship.ForeignDBName = polymorphicField.DBName
								relationship.PolymorphicType = polymorphicType.Name
								relationship.PolymorphicDBName = polymorphicType.DBName
								polymorphicType.IsForeignKey = true
								polymorphicField.IsForeignKey = true
							}
						}
					}

					switch indirectType.Kind() {
					case reflect.Slice:
						elemType := indirectType.Elem()
						if elemType.Kind() == reflect.Ptr {
							elemType = elemType.Elem()
						}

						if elemType.Kind() == reflect.Struct {
							if foreignKey == "" {
								foreignKey = scopeType.Name() + "Id"
							}

							if many2many := gormSettings["MANY2MANY"]; many2many != "" {
								relationship.Kind = "many_to_many"
								associationForeignKey := gormSettings["ASSOCIATIONFOREIGNKEY"]
								if associationForeignKey == "" {
									associationForeignKey = elemType.Name() + "Id"
								}

								relationship.ForeignFieldName = foreignKey
								relationship.ForeignDBName = ToDBName(foreignKey)
								relationship.AssociationForeignFieldName = associationForeignKey
								relationship.AssociationForeignDBName = ToDBName(associationForeignKey)

								joinTableHandler := JoinTableHandler{}
								joinTableHandler.Setup(relationship, many2many, scopeType, elemType)
								relationship.JoinTableHandler = &joinTableHandler
								field.Relationship = relationship
							} else {
								relationship.Kind = "has_many"
								if foreignField := getForeignField(foreignKey, toScope.GetStructFields()); foreignField != nil {
									relationship.ForeignFieldName = foreignField.Name
									relationship.ForeignDBName = foreignField.DBName
									foreignField.IsForeignKey = true
									field.Relationship = relationship
								} else if relationship.ForeignFieldName != "" {
									field.Relationship = relationship
								}
							}
						} else {
							field.IsNormal = true
						}
					case reflect.Struct:
						if embType, ok := gormSettings["EMBEDDED"]; ok || fieldStruct.Anonymous {
							for _, toField := range toScope.GetStructFields() {
								toField = toField.clone()
								if (embType == "prefixed") {
									toField.DBName = field.DBName+"__"+toField.DBName
								}
								toField.Names  = append([]string{fieldStruct.Name}, toField.Names...)
								modelStruct.StructFields = append(modelStruct.StructFields, toField)
								if _, ok := gormSettings["DROP_UNIQUE_INDEX"]; ok {
									toField.IgnoreUniqueIndex = true
								}
								if _, ok := gormSettings["DROP_PRIMARY_KEYS"]; ok {
									toField.IgnorePrimaryKey  = true
								}

								if toField.IsPrimaryKey && !toField.IgnorePrimaryKey {
									modelStruct.PrimaryFields = append(modelStruct.PrimaryFields, toField)
								}
							}
							continue
						} else {
							belongsToForeignKey := foreignKey
							if belongsToForeignKey == "" {
								belongsToForeignKey = field.Name + "Id"
							}

							if foreignField := getForeignField(belongsToForeignKey, fields); foreignField != nil {
								relationship.Kind = "belongs_to"
								relationship.ForeignFieldName = foreignField.Name
								relationship.ForeignDBName = foreignField.DBName
								foreignField.IsForeignKey = true
								field.Relationship = relationship
							} else {
								if foreignKey == "" {
									foreignKey = modelStruct.ModelType.Name() + "Id"
								}
								relationship.Kind = "has_one"
								if foreignField := getForeignField(foreignKey, toScope.GetStructFields()); foreignField != nil {
									relationship.ForeignFieldName = foreignField.Name
									relationship.ForeignDBName = foreignField.DBName
									foreignField.IsForeignKey = true
									field.Relationship = relationship
								} else if relationship.ForeignFieldName != "" {
									field.Relationship = relationship
								}
							}
						}
					default:
						field.IsNormal = true
					}
				}

				if field.IsNormal {
					if len(modelStruct.PrimaryFields) == 0 && field.DBName == "id" {
						field.IsPrimaryKey = true
						modelStruct.PrimaryFields = append(modelStruct.PrimaryFields, field)
					}
				}
			}
			modelStruct.StructFields = append(modelStruct.StructFields, field)
		}
		modelStructs_cacheMutex.Lock()
		modelStruct_last = &modelStruct
		modelStructs_cacheMutex.Unlock()
	}()

	if (cachable_byScopeType) {
		modelStructs_cacheMutex.Lock()
		modelStructs_byScopeType[scopeType] = &modelStruct
		modelStructs_cacheMutex.Unlock()
	} else {
		modelStructs_cacheMutex.Lock()
		modelStructs_byTableName[tableName] = &modelStruct
		modelStructs_cacheMutex.Unlock()
	}
	return &modelStruct
}

func (scope *Scope) GetStructFields() (fields []*StructField) {
	return scope.GetModelStruct().StructFields
}

func (scope *Scope) generateSqlTag(field *StructField) string {
	var sqlType string
	structType := field.Struct.Type
	if structType.Kind() == reflect.Ptr {
		structType = structType.Elem()
	}
	reflectValue := reflect.Indirect(reflect.New(structType))
	sqlSettings := parseTagSetting(field.Tag.Get("sql"))

	if value, ok := sqlSettings["TYPE"]; ok {
		sqlType = value
	}

	additionalType := sqlSettings["NOT NULL"]
	if !field.IgnoreUniqueIndex {
		additionalType = " " + sqlSettings["UNIQUE"]
	}
	if value, ok := sqlSettings["DEFAULT"]; ok {
		additionalType = additionalType + " DEFAULT " + value
	}

	if field.IsScanner {
		var getScannerValue func(reflect.Value)
		getScannerValue = func(value reflect.Value) {
			reflectValue = value
			if _, isScanner := reflect.New(reflectValue.Type()).Interface().(sql.Scanner); isScanner && reflectValue.Kind() == reflect.Struct {
				getScannerValue(reflectValue.Field(0))
			}
		}
		getScannerValue(reflectValue)
	}

	if sqlType == "" {
		var size = 255

		if value, ok := sqlSettings["SIZE"]; ok {
			size, _ = strconv.Atoi(value)
		}

		_, autoIncrease := sqlSettings["AUTO_INCREMENT"]
		if field.IsPrimaryKey {
			autoIncrease = true
		}

		sqlType = scope.Dialect().SqlTag(reflectValue, size, autoIncrease)
	}

	if strings.TrimSpace(additionalType) == "" {
		return sqlType
	} else {
		return fmt.Sprintf("%v %v", sqlType, additionalType)
	}
}

func parseTagSetting(str string) map[string]string {
	tags := strings.Split(str, ";")
	setting := map[string]string{}
	for _, value := range tags {
		v := strings.Split(value, ":")
		k := strings.TrimSpace(strings.ToUpper(v[0]))
		if len(v) == 2 {
			setting[k] = v[1]
		} else {
			setting[k] = k
		}
	}
	return setting
}
