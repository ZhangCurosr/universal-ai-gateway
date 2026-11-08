package obs

import "testing"

func TestMaskSecret(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "****"},
		{"12345678", "****"},
		{"sk-ant-api03-abcdefgh", "sk-a****efgh"},
	}
	for _, tt := range tests {
		if got := MaskSecret(tt.in); got != tt.want {
			t.Errorf("MaskSecret(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
