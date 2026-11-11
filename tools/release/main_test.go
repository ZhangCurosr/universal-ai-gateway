package main

import (
	"reflect"
	"testing"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"tagged release", "v0.3.0", "v0.3.0"},
		{"tagged with dirty", "v0.3.0-dirty", "v0.3.0-dirty"},
		{"tagged commit offset", "v0.3.0-4-g1234567", "v0.3.0-4-g1234567"},
		{"bare commit hash", "a7427aa", "v0.0.0-a7427aa"},
		{"bare commit hash with dirty", "a7427aa-dirty", "v0.0.0-a7427aa-dirty"},
		{"trimmed whitespace", "  v0.3.0 \n", "v0.3.0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseVersion(tt.in)
			if got != tt.want {
				t.Errorf("parseVersion(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestArtifactFilename(t *testing.T) {
	tests := []struct {
		name    string
		version string
		goos    string
		goarch  string
		want    string
	}{
		{"linux amd64", "v0.3.0", "linux", "amd64", "universal-ai-gateway_v0.3.0_linux_amd64"},
		{"linux arm64", "v0.3.0", "linux", "arm64", "universal-ai-gateway_v0.3.0_linux_arm64"},
		{"windows amd64", "v0.3.0", "windows", "amd64", "universal-ai-gateway_v0.3.0_windows_amd64.exe"},
		{"darwin arm64", "v0.3.0", "darwin", "arm64", "universal-ai-gateway_v0.3.0_darwin_arm64"},
		{"dev version with hash", "v0.0.0-a7427aa", "linux", "amd64", "universal-ai-gateway_v0.0.0-a7427aa_linux_amd64"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := artifactFilename(tt.version, tt.goos, tt.goarch)
			if got != tt.want {
				t.Errorf("artifactFilename(%q, %q, %q) = %q, want %q", tt.version, tt.goos, tt.goarch, got, tt.want)
			}
		})
	}
}

func TestFormatChecksumLine(t *testing.T) {
	tests := []struct {
		name     string
		sumHex   string
		filename string
		want     string
	}{
		{
			"standard lowercase",
			"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
			"universal-ai-gateway_v0.3.0_linux_amd64",
			"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad  universal-ai-gateway_v0.3.0_linux_amd64\n",
		},
		{
			"uppercase converted to lowercase",
			"BA7816BF8F01CFEA414140DE5DAE2223B00361A396177A9CB410FF61F20015AD",
			"test.exe",
			"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad  test.exe\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatChecksumLine(tt.sumHex, tt.filename)
			if got != tt.want {
				t.Errorf("formatChecksumLine(%q, %q) = %q, want %q", tt.sumHex, tt.filename, got, tt.want)
			}
		})
	}
}

func TestParsePlatforms(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []platform
		wantErr bool
	}{
		{
			"default all four",
			"linux/amd64,linux/arm64,windows/amd64,darwin/arm64",
			[]platform{
				{"linux", "amd64"},
				{"linux", "arm64"},
				{"windows", "amd64"},
				{"darwin", "arm64"},
			},
			false,
		},
		{
			"with whitespace",
			" linux/amd64 , windows/amd64 ",
			[]platform{
				{"linux", "amd64"},
				{"windows", "amd64"},
			},
			false,
		},
		{
			"invalid format",
			"linux",
			nil,
			true,
		},
		{
			"empty string",
			"",
			nil,
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePlatforms(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parsePlatforms(%q) error = %v, wantErr = %v", tt.in, err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parsePlatforms(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}
