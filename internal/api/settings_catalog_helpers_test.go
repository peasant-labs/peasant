package api

import (
	"github.com/peasant-labs/peasant/internal/config"
	"reflect"
	"time"
)

// filledConfig returns a configuration whose every field holds a value that
// differs from its zero value.
func filledConfig() config.Config {
	var cfg config.Config
	fill(reflect.ValueOf(&cfg).Elem())
	// The registry probe must use durations representable in config.yaml;
	// nanoseconds would round to zero during a field's config copy.
	cfg.Write.HoldTarget = time.Millisecond
	cfg.Write.HarvestTarget = time.Minute
	return cfg
}

func fill(v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Field(i).CanSet() {
				fill(v.Field(i))
			}
		}
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.String:
		v.SetString("filled")
	case reflect.Pointer:
		target := reflect.New(v.Type().Elem())
		fill(target.Elem())
		v.Set(target)
	case reflect.Slice:
		items := reflect.MakeSlice(v.Type(), 1, 1)
		fill(items.Index(0))
		v.Set(items)
	case reflect.Map:
		key := reflect.New(v.Type().Key()).Elem()
		value := reflect.New(v.Type().Elem()).Elem()
		fill(key)
		fill(value)
		entries := reflect.MakeMap(v.Type())
		entries.SetMapIndex(key, value)
		v.Set(entries)
	}
}
