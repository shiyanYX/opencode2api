package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestV5StatePathFor(t *testing.T) {
	cases := []struct {
		config string
		want   string
	}{
		{"", "v5_state.json"},
		{"/tmp/x/config.json", "/tmp/x/v5_state.json"},
		{"/data/config.json", "/data/v5_state.json"},
		{"config.json", "v5_state.json"},
	}
	for _, tc := range cases {
		if got := v5StatePathFor(tc.config); got != tc.want {
			t.Fatalf("v5StatePathFor(%q)=%q want %q", tc.config, got, tc.want)
		}
	}
}

func TestV5OffDoesNotCreateState(t *testing.T) {
	oldConfig := configPath
	dir := t.TempDir()
	configPath = filepath.Join(dir, "config.json")
	t.Cleanup(func() { configPath = oldConfig; resetV5ForTest() })
	t.Setenv("NODE_SYSTEM", "off")
	resetV5ForTest()
	v5PersistProbeResults()
	path := v5StatePathFor(configPath)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("off mode created state %s: err=%v", path, err)
	}
}
