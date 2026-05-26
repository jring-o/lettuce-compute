package leaf

import "testing"

func TestGenerateSlug(t *testing.T) {
	tests := []struct {
		name string
		input string
		want  string
	}{
		{
			name:  "simple words",
			input: "My Cool Project",
			want:  "my-cool-project",
		},
		{
			name:  "leading and trailing spaces",
			input: "   spaces   ",
			want:  "spaces",
		},
		{
			name:  "special characters",
			input: "Special!@#Characters",
			want:  "special-characters",
		},
		{
			name:  "leading and trailing hyphens",
			input: "---leading-trailing---",
			want:  "leading-trailing",
		},
		{
			name:  "consecutive special chars collapse",
			input: "hello!!!world",
			want:  "hello-world",
		},
		{
			name:  "numbers preserved",
			input: "Project 42 Alpha",
			want:  "project-42-alpha",
		},
		{
			name:  "already lowercase",
			input: "already-lowercase",
			want:  "already-lowercase",
		},
		{
			name:  "mixed case and underscores",
			input: "Mixed_Case_Project",
			want:  "mixed-case-project",
		},
		{
			name:  "single word",
			input: "Hello",
			want:  "hello",
		},
		{
			name:  "empty string",
			input: "",
			want:  "untitled",
		},
		{
			name:  "only spaces",
			input: "   ",
			want:  "untitled",
		},
		{
			name:  "only special chars",
			input: "!@#$%^&*()",
			want:  "untitled",
		},
		{
			name:  "unicode characters stripped",
			input: "Ünïcödé Prójëct",
			want:  "n-c-d-pr-j-ct",
		},
		{
			name:  "tabs and newlines",
			input: "hello\tworld\nnew",
			want:  "hello-world-new",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GenerateSlug(tt.input)
			if got != tt.want {
				t.Errorf("GenerateSlug(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGenerateSlugTruncation(t *testing.T) {
	// 120 'a' characters should truncate to 100.
	long := ""
	for i := 0; i < 120; i++ {
		long += "a"
	}
	got := GenerateSlug(long)
	if len(got) > 100 {
		t.Errorf("GenerateSlug(long) length = %d, want <= 100", len(got))
	}
}

func TestGenerateSlugTruncationNoTrailingHyphen(t *testing.T) {
	// Build a name that produces a hyphen right at position 100.
	// 99 'a's + a space + more chars → slug will be "aaa...a-more" → truncated at 100.
	name := ""
	for i := 0; i < 99; i++ {
		name += "a"
	}
	name += " more stuff here"
	got := GenerateSlug(name)
	if len(got) > 100 {
		t.Errorf("length = %d, want <= 100", len(got))
	}
	if got[len(got)-1] == '-' {
		t.Errorf("slug ends with hyphen: %q", got)
	}
}
