package api

import (
	"strings"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/mlops"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

func testClusters() []*store.Cluster {
	return []*store.Cluster{
		{Name: "nature-disaster", Desc: "自然灾害应急处置集群", Status: store.ClusterOnline, Token: "secret-token-a"},
		{Name: "emergency", Desc: "应急指挥集群", Status: store.ClusterOffline, Token: "secret-token-b"},
	}
}

func TestResolveTargetCluster(t *testing.T) {
	clusters := testClusters()

	// 显式指定（大小写不敏感）优先
	c, cands := resolveTargetCluster(clusters, "EMERGENCY", "随便什么文本")
	if c == nil || c.Name != "emergency" || cands != nil {
		t.Fatalf("explicit resolve = %v, candidates %v; want emergency", c, cands)
	}

	// 消息文本含存量中文集群名（Name 包含匹配）
	chineseNamed := []*store.Cluster{{Name: "自然灾害集群", Status: store.ClusterOnline}}
	c, _ = resolveTargetCluster(chineseNamed, "", "自然灾害集群external-server服务接口报500，排查一下")
	if c == nil || c.Name != "自然灾害集群" {
		t.Fatalf("chinese name resolve = %v; want 自然灾害集群", c)
	}

	// ASCII 注册名 + 中文描述：用户说口语简称「自然灾害集群」而描述是
	// 「自然灾害应急处置集群」——4 字滑窗「自然灾害」命中，且不误伤
	// 「应急指挥集群」（其滑窗片段均未出现）
	c, _ = resolveTargetCluster(clusters, "", "自然灾害集群的external-server接口500了")
	if c == nil || c.Name != "nature-disaster" {
		t.Fatalf("desc resolve = %v; want nature-disaster", c)
	}

	// 用户完整复述描述 → 整串包含命中
	c, _ = resolveTargetCluster(clusters, "", "请排查自然灾害应急处置集群的故障")
	if c == nil || c.Name != "nature-disaster" {
		t.Fatalf("full desc resolve = %v; want nature-disaster", c)
	}

	// 多命中 → 不猜，返回候选
	multi := []*store.Cluster{
		{Name: "a", Desc: "自然灾害集群"},
		{Name: "b", Desc: "自然灾害集群"},
	}
	c, cands = resolveTargetCluster(multi, "", "自然灾害集群报500")
	if c != nil || len(cands) != 2 {
		t.Fatalf("multi resolve = %v, candidates %v; want nil + 2 candidates", c, cands)
	}

	// 零命中（与集群无关的提问）
	c, cands = resolveTargetCluster(clusters, "", "今天天气怎么样")
	if c != nil || cands != nil {
		t.Fatalf("zero resolve = %v, %v; want nil, nil", c, cands)
	}
}

func TestBuildChatSystemPrefix(t *testing.T) {
	h := &Handlers{} // mlopsSvc nil → 走代码内置组装
	clusters := testClusters()

	// 命中目标：含目录、目标声明与守则
	target, _ := resolveTargetCluster(clusters, "", "自然灾害集群接口500")
	prefix := h.chatSystemPrefix(buildChatData(clusters, target, nil, ""))
	for _, want := range []string{"纳管集群目录", "nature-disaster", "emergency", "【本次目标集群】nature-disaster", "工具使用守则", "check_http"} {
		if !strings.Contains(prefix, want) {
			t.Errorf("prefix missing %q\n---\n%s", want, prefix)
		}
	}

	// 敏感字段不得进模型上下文
	if strings.Contains(prefix, "secret-token") {
		t.Errorf("prefix leaked token:\n%s", prefix)
	}

	// 连接失败：注入限制声明
	failed := h.chatSystemPrefix(buildChatData(clusters, target, nil, "dial tcp: timeout"))
	if !strings.Contains(failed, "连接失败") || !strings.Contains(failed, "dial tcp: timeout") {
		t.Errorf("connect-failure note missing:\n%s", failed)
	}

	// 多候选：要求澄清且不指定目标
	_, cands := resolveTargetCluster(clusters, "", "")
	multi := h.chatSystemPrefix(buildChatData(clusters, nil, cands, ""))
	if multi == "" {
		t.Fatal("multi-candidate prefix empty")
	}

	// 未解析到：引导澄清，不得出现目标声明
	none := h.chatSystemPrefix(buildChatData(clusters, nil, nil, ""))
	if strings.Contains(none, "【本次目标集群】") {
		t.Errorf("unresolved prefix must not declare target:\n%s", none)
	}
}

// TestChatSystemTemplateMatchesBuiltin chat_system 内置模板与代码内置组装
// 必须逐字节一致（mlops 侧模板路径与 api 侧回退路径等价）。
func TestChatSystemTemplateMatchesBuiltin(t *testing.T) {
	clusters := testClusters()
	target, _ := resolveTargetCluster(clusters, "", "自然灾害集群接口500")
	multiSrc := []*store.Cluster{
		{Name: "a", Desc: "自然灾害集群"},
		{Name: "b", Desc: "自然灾害集群"},
	}
	_, mc := resolveTargetCluster(multiSrc, "", "自然灾害集群报500")

	variants := []mlops.ChatData{
		buildChatData(clusters, target, nil, ""),
		buildChatData(clusters, target, nil, "dial tcp: timeout"),
		buildChatData(clusters, nil, mc, ""),
		buildChatData(clusters, nil, nil, ""),
		buildChatData(nil, nil, nil, ""),
	}
	for i, v := range variants {
		got, err := mlops.RenderBuiltinMessages(mlops.ScenarioChatSystem, &v)
		if err != nil {
			t.Fatalf("variant %d render: %v", i, err)
		}
		want := builtinChatSystemPrefix(v)
		if got != want {
			t.Errorf("variant %d template/builtin mismatch:\n--- template ---\n%q\n--- builtin ---\n%q", i, got, want)
		}
	}
}

func TestChatUserText(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": "自然灾害集群接口500"},
		map[string]any{"role": "assistant", "content": "好的"},
		map[string]any{"role": "user", "content": "继续排查"},
		"garbage",
	}
	got := chatUserText(msgs)
	for _, want := range []string{"自然灾害集群接口500", "继续排查"} {
		if !strings.Contains(got, want) {
			t.Errorf("chatUserText missing %q, got %q", want, got)
		}
	}
	if strings.Contains(got, "好的") || strings.Contains(got, "sys") {
		t.Errorf("chatUserText should only concat user messages, got %q", got)
	}
	if chatUserText(nil) != "" || chatUserText("not-a-list") != "" {
		t.Error("chatUserText must tolerate non-list input")
	}
}
