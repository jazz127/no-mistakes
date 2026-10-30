package config

import "testing"

func TestRequiredReviewsConfiguration(t *testing.T) {
	t.Parallel()
	const on = "ci:\n  required_reviews: [coderabbit]\n"
	const off = "ci:\n  required_reviews: []\n"
	for _, tc := range []struct {
		name, global, trusted, pushed string
		want                          int
	}{
		{"default", "{}", "{}", "{}", 0}, {"global", on, "{}", "{}", 1}, {"trusted", "{}", on, off, 1},
		{"trusted opt out", on, off, on, 0}, {"pushed cannot enable", "{}", "{}", on, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			global, err := LoadGlobalFromBytes([]byte(tc.global))
			if err != nil {
				t.Fatal(err)
			}
			trusted, err := LoadRepoFromBytes([]byte(tc.trusted))
			if err != nil {
				t.Fatal(err)
			}
			pushed, err := LoadRepoFromBytes([]byte(tc.pushed))
			if err != nil {
				t.Fatal(err)
			}
			for _, optin := range []bool{false, true} {
				got := Merge(global, EffectiveRepoConfig(pushed, trusted, optin)).CI.RequiredReviews
				if len(got) != tc.want {
					t.Fatalf("optin=%v got %v, want %d requirements", optin, got, tc.want)
				}
			}
		})
	}
	for _, bad := range []string{"[other]", "[coderabbit, coderabbit]", "[CodeRabbit]", "[123]", "coderabbit"} {
		data := []byte("ci:\n  required_reviews: " + bad + "\n")
		if _, err := LoadGlobalFromBytes(data); err == nil {
			t.Errorf("global accepted %s", bad)
		}
		if _, err := LoadRepoFromBytes(data); err == nil {
			t.Errorf("repo accepted %s", bad)
		}
	}
}
