package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestModelConfigEnabledNormalization 旧配置缺失 enabled 归一化为 true，
// 显式 false 保留、显式 true 不变。
func TestModelConfigEnabledNormalization(t *testing.T) {
	var c Config
	src := `
providers:
  - name: p
    type: openai_compatible
    base_url: http://127.0.0.1:1
    api_key: k
    models:
      - name: m-default      # 旧格式：无 enabled → true
      - name: m-off
        enabled: false
      - name: m-on
        enabled: true
`
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	models := c.Providers[0].Models
	if len(models) != 3 {
		t.Fatalf("expected 3 models, got %d", len(models))
	}
	for i, want := range []bool{true, false, true} {
		if models[i].Enabled != want {
			t.Fatalf("models[%d].Enabled = %v, want %v", i, models[i].Enabled, want)
		}
	}
	if models[0].Name != "m-default" {
		t.Fatalf("field lost after normalization: %+v", models[0])
	}
}

// TestModelConfigRoundtripAfterNormalize 归一化后序列化再反序列化：
// enabled 语义稳定（缺失 → true 落盘后仍为 true）。
func TestModelConfigRoundtripAfterNormalize(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("providers:\n  - name: p\n    models:\n      - name: m\n"), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !c.Providers[0].Models[0].Enabled {
		t.Fatal("missing enabled should normalize to true")
	}
	out, err := yaml.Marshal(&c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var c2 Config
	if err := yaml.Unmarshal(out, &c2); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if !c2.Providers[0].Models[0].Enabled {
		t.Fatal("enabled lost after roundtrip")
	}
}
