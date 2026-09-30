package http

import (
	"reflect"
	"testing"

	"github.com/tiim/photo-collect/internal/database/sqlc"
)

// imageOf copies field by field; adding a column to images must not be
// forgotten there.
func TestImageOfCoversEveryColumn(t *testing.T) {
	row := reflect.ValueOf(&sqlc.ListImagesFilteredRow{}).Elem()
	img := reflect.TypeOf(sqlc.Image{})
	extra := map[string]bool{"SortKey": true, "IsDesc": true}
	if got, want := row.NumField()-len(extra), img.NumField(); got != want {
		t.Fatalf("filtered row has %d image columns, sqlc.Image has %d", got, want)
	}
	// Fill every column with a distinct non-zero value and check it survives.
	for i := 0; i < row.NumField(); i++ {
		f := row.Field(i)
		switch f.Kind() {
		case reflect.Int64:
			f.SetInt(int64(i + 1))
		case reflect.String:
			f.SetString(row.Type().Field(i).Name)
		case reflect.Struct:
			f.Field(0).Set(reflect.Zero(f.Field(0).Type()))
			switch v := f.Field(0); v.Kind() {
			case reflect.String:
				v.SetString("x")
			case reflect.Int64:
				v.SetInt(int64(i + 1))
			case reflect.Float64:
				v.SetFloat(float64(i + 1))
			}
			f.Field(1).SetBool(true)
		}
	}
	out := reflect.ValueOf(imageOf(row.Interface().(sqlc.ListImagesFilteredRow)))
	for i := 0; i < img.NumField(); i++ {
		if out.Field(i).IsZero() {
			t.Errorf("imageOf drops %s", img.Field(i).Name)
		}
	}
}
