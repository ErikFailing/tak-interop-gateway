// Package jsonpath builds the JSON paths the project's reports name fields by, such as location.position.lat or
// aliases.alternateIds[0].id. A plain key joins with a dot; any other key, an empty one or one containing a dot
// included, is quoted in brackets, so every path is non-empty and reads back unambiguously.
package jsonpath

import (
	"fmt"
	"strconv"
)

// Key appends an object key to path.
func Key(path, key string) string {
	switch {
	case !plain(key):
		return path + "[" + strconv.Quote(key) + "]"
	case path == "":
		return key
	}
	return path + "." + key
}

// Index appends an array index to path.
func Index(path string, i int) string {
	return fmt.Sprintf("%s[%d]", path, i)
}

// plain reports whether key is non-empty and only ASCII letters, digits and underscores, as every SDK field name is.
func plain(key string) bool {
	if key == "" {
		return false
	}
	for i := range len(key) {
		if b := key[i]; b != '_' && (b < 'a' || b > 'z') && (b < 'A' || b > 'Z') && (b < '0' || b > '9') {
			return false
		}
	}
	return true
}
