package memory

import "testing"

func FuzzMapPersonaDirectiveField(f *testing.F) {
	f.Add("name")
	f.Add("communication style")
	f.Add("something custom")

	f.Fuzz(func(t *testing.T, field string) {
		path, op := mapPersonaDirectiveField(field)
		if path == "" {
			t.Fatalf("expected non-empty path for %q", field)
		}
		if op != "set" && op != "append" {
			t.Fatalf("unexpected operation %q", op)
		}
	})
}

func FuzzDeriveYouAreCandidatesNoPanic(f *testing.F) {
	f.Add("my personal assistant")
	f.Add("casual and direct")
	f.Add("blonde with ponytail")

	f.Fuzz(func(t *testing.T, clause string) {
		_ = deriveYouAreCandidates("s", "t", "u", "a", "evt", clause, clause)
	})
}
