package config

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every field a probe has must reach the merged probe: one left out of
// mergeFrom is silently dropped on every probe, template or not.
func TestMergeFromCoversEveryField(t *testing.T) {
	typ := reflect.TypeFor[Probe]()
	for i := range typ.NumField() {
		field := typ.Field(i)
		var other Probe
		fill(reflect.ValueOf(&other).Elem().Field(i))

		got := reflect.ValueOf(Probe{}.mergeFrom(other)).Field(i).Interface()
		if want := reflect.ValueOf(other).Field(i).Interface(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s set on the probe is lost by mergeFrom: got %v, want %v", field.Name, got, want)
		}
	}
}

// fill gives v a non-zero value, whatever its type.
func fill(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Int, reflect.Int64:
		v.SetInt(1)
	case reflect.Float64:
		v.SetFloat(1)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem())
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(v.Index(0))
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		key, elem := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fill(key)
		fill(elem)
		v.SetMapIndex(key, elem)
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[yaml.Node]() {
			v.Set(reflect.ValueOf(yaml.Node{Kind: yaml.ScalarNode, Value: "x"}))
			return
		}
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i))
			}
		}
	}
}

func TestHostnameSurvivesInheritance(t *testing.T) {
	p, err := ParseProbes([]byte(`
templates:
  web: {type: http, hostname: from-template.test, http: {}}
probes:
  - {name: own, type: http, hostname: own.test, targets: ["https://1.2.3.4/"], http: {}}
  - {name: tpl, template: web, targets: ["https://1.2.3.4/"]}
`))
	if err != nil {
		t.Fatal(err)
	}
	list, err := p.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Hostname != "own.test" {
		t.Errorf("the probe's own hostname is lost: %q", list[0].Hostname)
	}
	if list[1].Hostname != "from-template.test" {
		t.Errorf("the template's hostname is lost: %q", list[1].Hostname)
	}
}
