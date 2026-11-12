package dynamodb

import (
	"encoding/json"
	"testing"
)

func mustItem(t *testing.T, raw string) item {
	t.Helper()
	var it item
	if err := json.Unmarshal([]byte(raw), &it); err != nil {
		t.Fatal(err)
	}
	return it
}

func TestConditionExpressions(t *testing.T) {
	it := mustItem(t, `{"id":{"S":"a1"},"n":{"N":"10"},"tags":{"SS":["x","y"]},
		"m":{"M":{"deep":{"L":[{"N":"1"},{"S":"two"}]}}},"flag":{"BOOL":true},"empty":{"NULL":true}}`)
	values := map[string]any{}
	_ = json.Unmarshal([]byte(`{":ten":{"N":"10.0"},":five":{"N":"5"},":a":{"S":"a"},":x":{"S":"x"},
		":two":{"S":"two"},":S":{"S":"S"},":big":{"N":"100"},":ss":{"S":"SS"}}`), &values)
	cases := []struct {
		expr string
		want bool
	}{
		{"n = :ten", true},
		{"n <> :ten", false},
		{"n BETWEEN :five AND :big", true},
		{"n IN (:five, :big)", false},
		{"begins_with(id, :a)", true},
		{"contains(tags, :x)", true},
		{"contains(m.deep, :two)", true},
		{"m.deep[1] = :two", true},
		{"attribute_exists(m.deep[5])", false},
		{"attribute_type(id, :S)", true},
		{"attribute_type(tags, :ss)", true},
		{"size(tags) > :five", false},
		{"size(id) < :five", true},
		{"NOT (n < :five) AND (flag = flag OR absent = :a)", true},
		{"absent < :big", false},
		{"attribute_not_exists(absent)", true},
		{"n > id", false}, // different types never compare
	}
	for _, c := range cases {
		cond, err := parseCondition(c.expr, newExprContext(nil, values))
		if err != nil {
			t.Fatalf("%q: %v", c.expr, err)
		}
		got, err := cond.eval(it)
		if err != nil {
			t.Fatalf("%q: eval: %v", c.expr, err)
		}
		if got != c.want {
			t.Errorf("%q = %v, want %v", c.expr, got, c.want)
		}
	}
}

func TestConditionSyntaxErrors(t *testing.T) {
	values := map[string]any{":v": map[string]any{"S": "x"}}
	for _, expr := range []string{
		"a = ",
		"a = :absent",
		"#undefined = :v",
		"a == :v",
		"begins_with(a)",
		"size = :v",
		"a = :v AND",
		"(a = :v",
	} {
		if _, err := parseCondition(expr, newExprContext(nil, values)); err == nil {
			t.Errorf("%q parsed, want an error", expr)
		}
	}
}

func TestUnusedPlaceholdersAreReported(t *testing.T) {
	ctx := newExprContext(map[string]string{"#a": "a", "#b": "b"}, map[string]any{":v": map[string]any{"S": "x"}})
	if _, err := parseCondition("#a = :v", ctx); err != nil {
		t.Fatal(err)
	}
	if err := ctx.unused(); err == nil {
		t.Fatal("unused #b was not reported")
	}
}

func TestUpdateEvaluatesAgainstTheOriginalItem(t *testing.T) {
	values := map[string]any{}
	_ = json.Unmarshal([]byte(`{":one":{"N":"1"}}`), &values)
	u, err := parseUpdate("SET a = b, b = a ADD c :one", newExprContext(nil, values))
	if err != nil {
		t.Fatal(err)
	}
	got, err := u.apply(mustItem(t, `{"a":{"N":"1"},"b":{"N":"2"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got["a"].(map[string]any)["N"] != "2" || got["b"].(map[string]any)["N"] != "1" || got["c"].(map[string]any)["N"] != "1" {
		t.Fatalf("swap/ADD = %v", got)
	}
}

func TestUpdateRejectsBadOperands(t *testing.T) {
	values := map[string]any{}
	_ = json.Unmarshal([]byte(`{":s":{"S":"x"},":n":{"N":"1"},":ss":{"SS":["a"]}}`), &values)
	for _, c := range []struct{ expr, item string }{
		{"SET a = a + :s", `{"a":{"N":"1"}}`},  // arithmetic on a string
		{"ADD a :n", `{"a":{"S":"x"}}`},        // ADD to a string
		{"DELETE a :ss", `{"a":{"NS":["1"]}}`}, // set type mismatch
		{"SET a = list_append(a, :n)", `{"a":{"L":[]}}`},
	} {
		u, err := parseUpdate(c.expr, newExprContext(nil, values))
		if err != nil {
			continue // rejected at parse time is fine too
		}
		if _, err := u.apply(mustItem(t, c.item)); err == nil {
			t.Errorf("%q on %s succeeded, want an error", c.expr, c.item)
		}
	}
}

func TestNumbersCompareByValue(t *testing.T) {
	a := map[string]any{"N": "100"}
	b := map[string]any{"N": "1E2"}
	if c, ok := compareValues(a, b); !ok || c != 0 {
		t.Fatalf("100 vs 1E2 = %d, %v", c, ok)
	}
	if canonicalKey(a) != canonicalKey(b) {
		t.Fatal("equal numbers must share a storage key")
	}
	if c, _ := compareValues(map[string]any{"N": "9"}, map[string]any{"N": "10"}); c >= 0 {
		t.Fatal("9 must sort before 10")
	}
}
