// Package set holds zemi's one set type.
package set

import (
	"maps"
	"slices"
)

// Set is a set of T. It is a map underneath, so range and len read it
// directly; writes go through Add and Remove.
type Set[T comparable] map[T]struct{}

// Of builds a set of its arguments.
func Of[T comparable](items ...T) Set[T] {
	s := make(Set[T], len(items))
	for _, it := range items {
		s[it] = struct{}{}
	}
	return s
}

func (s Set[T]) Add(item T)      { s[item] = struct{}{} }
func (s Set[T]) Remove(item T)   { delete(s, item) }
func (s Set[T]) Has(item T) bool { _, ok := s[item]; return ok }

// Clone copies the set, so the two can be written independently.
func (s Set[T]) Clone() Set[T] {
	out := make(Set[T], len(s))
	for item := range s {
		out[item] = struct{}{}
	}
	return out
}

// Slice returns the members in map order.
func (s Set[T]) Slice() []T {
	return slices.Collect(maps.Keys(s))
}
