package llm

import "testing"

func TestValidProvider(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"bedrock", ProviderBedrock, true},
		{"ark", ProviderArk, true},
		{"空字符串", "", false},
		{"未知 provider", "openai", false},
		{"大小写敏感", "Bedrock", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ValidProvider(tc.input); got != tc.want {
				t.Fatalf("ValidProvider(%q) = %v，期望 %v", tc.input, got, tc.want)
			}
		})
	}
}
