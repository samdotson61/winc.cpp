package journal

import "testing"

func msgsOf(texts ...string) []Msg {
	out := make([]Msg, len(texts))
	role := "user"
	for i, t := range texts {
		out[i] = Msg{Role: role, Text: t, RawLen: len(t) + 30}
		if role == "user" {
			role = "assistant"
		} else {
			role = "user"
		}
	}
	return out
}

func TestBuildChainDeterministic(t *testing.T) {
	a := BuildChain(msgsOf("hola", "hola de nuevo", "¿qué tal?"))
	b := BuildChain(msgsOf("hola", "hola de nuevo", "¿qué tal?"))
	if len(a) != 3 {
		t.Fatalf("chain length: want 3, got %d", len(a))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("chain not deterministic at %d: %s vs %s", i, a[i], b[i])
		}
	}
}

func TestChainRoleMatters(t *testing.T) {
	a := BuildChain([]Msg{{Role: "user", Text: "hi"}})
	b := BuildChain([]Msg{{Role: "assistant", Text: "hi"}})
	if a[0] == b[0] {
		t.Fatal("same text under different roles must not collide")
	}
	// The separator byte keeps (role,text) unambiguous.
	c := BuildChain([]Msg{{Role: "user", Text: "ab"}})
	d := BuildChain([]Msg{{Role: "usera", Text: "b"}})
	if c[0] == d[0] {
		t.Fatal("role/text boundary must not be ambiguous")
	}
}

func TestMatchLen(t *testing.T) {
	base := msgsOf("a", "b", "c", "d", "e", "f")
	full := BuildChain(base)
	cases := []struct {
		name string
		a, b Chain
		want int
	}{
		{"identical", full, BuildChain(base), 6},
		{"prefix", full[:3], full, 3},
		{"empty", nil, full, 0},
		{"diverges at 2", full, BuildChain(msgsOf("a", "b", "X", "d")), 2},
		{"diverges at 0", full, BuildChain(msgsOf("Z")), 0},
		{"diverges at last", full, BuildChain(msgsOf("a", "b", "c", "d", "e", "X")), 5},
	}
	for _, tc := range cases {
		if got := matchLen(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: want %d, got %d", tc.name, tc.want, got)
		}
	}
}
