package tags

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestFromQueryReadsTagsInIndexOrder(t *testing.T) {
	form := url.Values{
		"Tags.member.2.Key": {"team"}, "Tags.member.2.Value": {"core"},
		"Tags.member.1.Key": {"env"}, "Tags.member.1.Value": {"dev"},
		"Other": {"x"},
	}
	got, err := FromQuery(form, "Tags.member.")
	if err != nil {
		t.Fatal(err)
	}
	want := List{{"env", "dev"}, {"team", "core"}}
	if !got.Equal(want) || got[0].Key != "env" {
		t.Fatalf("got %v, want %v in order", got, want)
	}
}

func TestFromQueryRefusesDuplicateKeys(t *testing.T) {
	form := url.Values{"Tags.member.1.Key": {"a"}, "Tags.member.2.Key": {"a"}}
	_, err := FromQuery(form, "Tags.member.")
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Reason != DuplicateKey {
		t.Fatalf("got %v, want a DuplicateKey error", err)
	}
}

func TestFromQueryFieldsUsesCustomNames(t *testing.T) {
	form := url.Values{"Tag.1.TagKey": {"k"}, "Tag.1.TagValue": {"v"}}
	got, err := FromQueryFields(form, "Tag.", "TagKey", "TagValue")
	if err != nil || !got.Equal(List{{"k", "v"}}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestKeysFromQueryStopsAtFirstGap(t *testing.T) {
	form := url.Values{"TagKeys.member.1": {"a"}, "TagKeys.member.2": {"b"}, "TagKeys.member.4": {"d"}}
	got := KeysFromQuery(form, "TagKeys.member.")
	if strings.Join(got, ",") != "a,b" {
		t.Fatalf("got %v", got)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		list List
		want Reason
	}{
		{"empty key", List{{"", "v"}}, KeyLength},
		{"long key", List{{strings.Repeat("k", 129), ""}}, KeyLength},
		{"long value", List{{"k", strings.Repeat("v", 257)}}, ValueLength},
		{"reserved prefix", List{{"AWS:cloudformation", "v"}}, ReservedPrefix},
		{"too many", List{{"a", ""}, {"b", ""}}, TooMany},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var verr *ValidationError
			if err := tc.list.Validate(1); !errors.As(err, &verr) || verr.Reason != tc.want {
				t.Fatalf("got %v, want reason %d", err, tc.want)
			}
		})
	}
	if err := (List{{"k", ""}}).Validate(MaxPerResource); err != nil {
		t.Fatalf("an empty value is valid: %v", err)
	}
}

func TestMergeOverwritesInPlaceAndAppendsNewKeys(t *testing.T) {
	got := List{{"a", "1"}, {"b", "2"}}.Merge(List{{"b", "3"}, {"c", "4"}})
	want := List{{"a", "1"}, {"b", "3"}, {"c", "4"}}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestWithoutIgnoresAbsentKeys(t *testing.T) {
	got := List{{"a", "1"}, {"b", "2"}}.Without([]string{"a", "zzz"})
	if !got.Equal(List{{"b", "2"}}) {
		t.Fatalf("got %v", got)
	}
	if got := (List{{"a", "1"}}).Without([]string{"a"}); got == nil || len(got) != 0 {
		t.Fatalf("removing every tag leaves an empty, non-nil list: %#v", got)
	}
}

func TestEqualIgnoresOrder(t *testing.T) {
	if !(List{{"a", "1"}, {"b", "2"}}).Equal(List{{"b", "2"}, {"a", "1"}}) {
		t.Fatal("same pairs in another order should be equal")
	}
	if (List{{"a", "1"}}).Equal(List{{"a", "2"}}) {
		t.Fatal("different values should not be equal")
	}
}
