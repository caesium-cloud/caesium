package models

import (
	"reflect"
)

var (
	models = map[interface{}]*[]string{
		Atom{}:     &AtomColumns,
		Callback{}: &CallbackColumns,
		Job{}:      &JobColumns,
		Task{}:     &TaskColumns,
		Trigger{}:  &TriggerColumns,
	}
)

func init() {
	for model, columns := range models {
		buildColumns(model, columns)
	}
}

func buildColumns(model interface{}, columns *[]string) {
	t := reflect.TypeOf(model)
	for i := 0; i < t.NumField(); i++ {
		col, _ := t.Field(i).Tag.Lookup("db")
		*columns = append(*columns, col)
	}
}
