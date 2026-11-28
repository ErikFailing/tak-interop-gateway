package jsonpath_test

import (
	"testing"

	"github.com/erikfailing/tak-interop-gateway/internal/jsonpath"
)

func TestPaths(t *testing.T) {
	for _, tt := range []struct{ got, want string }{
		{jsonpath.Key("", "location"), "location"},
		{jsonpath.Key("location", "position"), "location.position"},
		{jsonpath.Key("", ""), `[""]`},
		{jsonpath.Key("aliases", "a.b"), `aliases["a.b"]`},
		{jsonpath.Key("x", "naïve"), `x["naïve"]`},
		{jsonpath.Index("aliases.alternateIds", 0), "aliases.alternateIds[0]"},
		{jsonpath.Key(jsonpath.Index("sensors.sensors", 2), "sensorType"), "sensors.sensors[2].sensorType"},
	} {
		if tt.got != tt.want {
			t.Errorf("got %q, want %q", tt.got, tt.want)
		}
	}
}
