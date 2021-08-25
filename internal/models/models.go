package models

import "reflect"

func init() {
	// atom columns
	{
		t := reflect.TypeOf(Atom{})
		for i := 0; i < t.NumField(); i++ {
			col, _ := t.Field(i).Tag.Lookup("db")
			AtomColumns = append(AtomColumns, col)
		}
	}
}
