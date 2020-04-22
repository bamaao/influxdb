package override

import (
	"context"
	"testing"

	"github.com/influxdata/influxdb/v2/kit/feature"
)

func TestFlagger(t *testing.T) {

	cases := []struct {
		name           string
		env            string
		defaults       []feature.Flag
		expected       map[string]interface{}
		expectMakeErr  bool
		expectFlagsErr bool
	}{
		{
			name: "enabled happy path filtering",
			env:  "flag1:new1,flag3:new3",
			defaults: []feature.Flag{
				newFlag("flag0", "original0"),
				newFlag("flag1", "original1"),
				newFlag("flag2", "original2"),
				newFlag("flag3", "original3"),
				newFlag("flag4", "original4"),
			},
			expected: map[string]interface{}{
				"flag0": "original0",
				"flag1": "new1",
				"flag2": "original2",
				"flag3": "new3",
				"flag4": "original4",
			},
		},
		{
			name: "enabled happy path types",
			env:  "intflag:43,floatflag:43.43,boolflag:true",
			defaults: []feature.Flag{
				newFlag("intflag", 42),
				newFlag("floatflag", 42.42),
				newFlag("boolflag", false),
			},
			expected: map[string]interface{}{
				"intflag":   43,
				"floatflag": 43.43,
				"boolflag":  true,
			},
		},
		{
			name:          "parse error",
			env:           "hello i am malformed, how are you?",
			expectMakeErr: true,
		},
		{
			name: "type coerce error",
			env:  "key:not_an_int",
			defaults: []feature.Flag{
				newFlag("key", 42),
			},
			expectFlagsErr: true,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			subject, err := Make(test.env)
			if err != nil {
				if test.expectMakeErr {
					return
				}
				t.Fatalf("unexpected error making Flagger: %v", err)
			}

			computed, err := subject.Flags(context.Background(), test.defaults...)
			if err != nil {
				if test.expectFlagsErr {
					return
				}
				t.Fatalf("unexpected error calling Flags: %v", err)
			}

			if len(computed) != len(test.expected) {
				t.Fatalf("incorrect number of flags computed: expected %d, got %d", len(test.expected), len(computed))
			}

			// check for extra or incorrect keys
			for k, v := range computed {
				if xv, found := test.expected[k]; !found {
					t.Errorf("unexpected key %s", k)
				} else if v != xv {
					t.Errorf("incorrect value for key %s: expected %v, got %v", k, xv, v)
				}
			}

			// check for missing keys
			for k := range test.expected {
				if _, found := computed[k]; !found {
					t.Errorf("missing expected key %s", k)
				}
			}
		})
	}
}

func newFlag(key string, defaultValue interface{}) feature.Flag {
	return feature.MakeFlag(key, key, "", defaultValue, feature.Temporary, false)
}
