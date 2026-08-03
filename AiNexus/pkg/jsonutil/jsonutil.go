package jsonutil

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ToJSONIndent 将对象格式化为缩进 JSON 字符串
func ToJSONIndent(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// ToJSON 将对象格式化为紧凑 JSON 字符串
func ToJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// PrettyJSON 美化 JSON 字符串
func PrettyJSON(s string) string {
	var buf strings.Builder
	if err := json.Indent(&buf, []byte(s), "", "  "); err != nil {
		return s
	}
	return buf.String()
}
