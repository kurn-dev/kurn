package main

// filterFlags collects repeatable -filter name=value pairs. The split is
// on the FIRST '=' so an empty value and values containing '=' remain
// representable; a missing separator and a repeated logical name are
// rejected at parse time, before any store is opened or query executed
// (a repeated name would otherwise silently last-wins).

import (
	"fmt"
	"strings"
)

type filterFlags struct {
	m map[string]string
}

func (f *filterFlags) String() string { return "" }

func (f *filterFlags) Set(v string) error {
	name, val, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("-filter needs name=value, got %q", v)
	}
	if name == "" {
		return fmt.Errorf("-filter needs a non-empty name, got %q", v)
	}
	if f.m == nil {
		f.m = map[string]string{}
	}
	if _, dup := f.m[name]; dup {
		return fmt.Errorf("-filter name %q repeated (values would silently last-wins)", name)
	}
	f.m[name] = val
	return nil
}
