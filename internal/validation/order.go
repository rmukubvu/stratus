package validation

import (
	"sort"

	"github.com/stratus/internal/awsmodel"
)

// sortedMembers orders a structure's members by name, so violations come out
// in a stable order.
func sortedMembers(shape *awsmodel.Shape) []string {
	names := make([]string, 0, len(shape.Members))
	for name := range shape.Members {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
