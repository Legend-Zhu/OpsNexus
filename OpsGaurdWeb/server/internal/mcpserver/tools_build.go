// 镜像与构建工具（image_* / build_*）：内嵌仓库清单/删除 + zip 构建提交与
// 跟踪。zip 不经 MCP 协议传输——固定流程 build_upload_begin → curl 直传
// （票据一次性）→ build_submit，见 upload.go。
package mcpserver

import (
	"context"
	"fmt"
	"time"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/registry"
)

type imageNameIn struct {
	Name    string `json:"name" description:"Image repo name (from image_list)"`
	Tag     string `json:"tag" description:"Tag to delete"`
	Confirm bool   `json:"confirm,omitempty" description:"Must be true for delete"`
}

type imageListIn struct {
	Name string `json:"name,omitempty" description:"Filter to one repo (default: all)"`
}

type imageListOut struct {
	Items []registry.RepoView `json:"items"`
	Hint  string              `json:"hint,omitempty"`
}

type uploadBeginIn struct {
	Filename  string `json:"filename" description:"Local zip file name, e.g. gw.zip"`
	SizeBytes int64  `json:"size_bytes" description:"Exact zip size in bytes (the upload is rejected unless it matches)"`
}

type uploadBeginOut struct {
	UploadID string `json:"upload_id"`
	Ticket   string `json:"ticket"`
	Curl     string `json:"curl" description:"Ready-to-run curl command for the direct upload"`
	Hint     string `json:"hint"`
}

type buildSubmitIn struct {
	UploadID   string `json:"upload_id" description:"upload_id from build_upload_begin (zip must already be uploaded)"`
	Name       string `json:"name" description:"Image repo name (lowercase letters/digits/_/.- segments)"`
	Tag        string `json:"tag" description:"Image tag"`
	Dockerfile string `json:"dockerfile,omitempty" description:"Path to the Dockerfile inside the zip (default: ./Dockerfile at zip root)"`
}

type buildOut struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Image  string `json:"image,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

type buildGetIn struct {
	ID       string `json:"id" description:"Build id from build_submit"`
	LogsTail int    `json:"logs_tail,omitempty" description:"Recent build log lines to include (default 40, max 200)"`
}

type buildGetOut struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Progress   int    `json:"progress"`
	Image      string `json:"image,omitempty"`
	Error      string `json:"error,omitempty"`
	LogsTail   string `json:"logs_tail,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	Hint       string `json:"hint,omitempty"`
}

type buildListIn struct {
	Limit int `json:"limit,omitempty" description:"Max entries (default 20)"`
}

type buildListOut struct {
	Items []buildOut `json:"items"`
}

// requireRegistry 构建类工具的前置：内嵌仓库未启用时给引导性错误。
func (h *Handler) requireRegistry() (*registry.Service, error) {
	if h.deps.Registry == nil {
		return nil, fmt.Errorf("embedded image registry is not enabled (set registry.enabled=true in config.yaml)")
	}
	return h.deps.Registry, nil
}

func (h *Handler) registerBuildTools(s *mcp.Server) {
	// image_list
	mcp.AddTool(s, &mcp.Tool{
		Name:        "image_list",
		Description: "List image repos and tags in the embedded registry. Cluster deploys pull images as registry.opsguard/<name>:<tag>.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in imageListIn) (*mcp.CallToolResult, imageListOut, error) {
		reg, err := h.requireRegistry()
		if err != nil {
			return nil, imageListOut{}, err
		}
		st := reg.Store()
		items := []registry.RepoView{}
		for _, repo := range st.Catalog() {
			if in.Name != "" && repo != in.Name {
				continue
			}
			tags, err := st.Tags(repo)
			if err != nil {
				continue
			}
			items = append(items, registry.RepoView{Name: repo, Tags: tags})
		}
		return nil, imageListOut{Items: items,
			Hint: "reference in deploy config as registry.opsguard/<name>:<tag>"}, nil
	})

	// image_delete
	mcp.AddTool(s, &mcp.Tool{
		Name:        "image_delete",
		Description: "Delete one tag of an image repo (manifests swept by GC). Destructive: requires confirm=true.",
	}, audited(h, "image_delete",
		func(in imageNameIn) map[string]string { return map[string]string{"name": in.Name, "tag": in.Tag} },
		func(ctx context.Context, in imageNameIn) (buildOut, error) {
			reg, err := h.requireRegistry()
			if err != nil {
				return buildOut{}, err
			}
			if !in.Confirm {
				return buildOut{}, fmt.Errorf("deleting image %s:%s cannot be undone (running services keep their local copy, "+
					"but re-pulls will fail); restate this impact to the user and set confirm=true to proceed", in.Name, in.Tag)
			}
			if !reg.Store().DeleteTag(in.Name, in.Tag) {
				return buildOut{}, fmt.Errorf("tag not found: %s:%s", in.Name, in.Tag)
			}
			reg.Store().SweepBlobs()
			return buildOut{Status: "deleted", Image: in.Name + ":" + in.Tag}, nil
		}))

	// build_upload_begin
	mcp.AddTool(s, &mcp.Tool{
		Name:        "build_upload_begin",
		Description: "Step 1 of building an image: register the zip package (name + exact size) and get a one-time upload ticket. Then run the returned curl command to upload the file directly (the zip never travels through MCP). Finally call build_submit with the upload_id.",
	}, audited(h, "build_upload_begin",
		func(in uploadBeginIn) map[string]string { return map[string]string{"filename": in.Filename} },
		func(ctx context.Context, in uploadBeginIn) (uploadBeginOut, error) {
			if _, err := h.requireRegistry(); err != nil {
				return uploadBeginOut{}, err
			}
			if in.SizeBytes <= 0 {
				return uploadBeginOut{}, fmt.Errorf("size_bytes must be the exact zip size in bytes (run: stat -c %%s <file> / ls -l)")
			}
			t, err := h.tickets.begin(in.Filename, in.SizeBytes, h.deps.Registry.MaxUploadMB())
			if err != nil {
				return uploadBeginOut{}, err
			}
			curl := fmt.Sprintf("curl -sS -X POST \"%s/api/v1/mcp/build-upload?upload_id=%s&ticket=%s\" "+
				"-H \"Content-Type: application/octet-stream\" --data-binary @%s",
				h.baseURL(ctx), t.ID, t.Ticket, in.Filename)
			return uploadBeginOut{
				UploadID: t.ID,
				Ticket:   t.Ticket,
				Curl:     curl,
				Hint:     "run the curl command from your shell (not through MCP), then build_submit{upload_id}",
			}, nil
		}))

	// build_submit
	mcp.AddTool(s, &mcp.Tool{
		Name:        "build_submit",
		Description: "Step 3 of building an image: submit the uploaded zip (from build_upload_begin + curl direct upload) to docker build & push. Returns a build id; poll build_get until SUCCESS/FAILED.",
	}, audited(h, "build_submit",
		func(in buildSubmitIn) map[string]string {
			return map[string]string{"upload_id": in.UploadID, "name": in.Name, "tag": in.Tag}
		},
		func(ctx context.Context, in buildSubmitIn) (buildOut, error) {
			reg, err := h.requireRegistry()
			if err != nil {
				return buildOut{}, err
			}
			if !reg.DockerAvailable() {
				return buildOut{}, fmt.Errorf("management server has no docker CLI on PATH; image build unavailable")
			}
			path, filename, err := h.tickets.take(in.UploadID)
			if err != nil {
				return buildOut{}, fmt.Errorf("%v (restart from build_upload_begin)", err)
			}
			task, err := reg.SubmitBuild(path, in.Name, in.Tag, in.Dockerfile)
			if err != nil {
				return buildOut{}, fmt.Errorf("%v (uploaded package %s kept for retry; resubmit with the same upload_id... "+
					"actually re-run build_upload_begin: this upload was consumed)", err, filename)
			}
			return buildOut{ID: task.ID, Status: string(task.Status), Image: task.Image,
				Hint: "poll build_get until status=SUCCESS, then deploy with image " + task.Image}, nil
		}))

	// build_get
	mcp.AddTool(s, &mcp.Tool{
		Name:        "build_get",
		Description: "Poll a build: status PENDING→EXTRACTING→BUILDING→PUSHING→CLEANING→SUCCESS|FAILED, progress, and recent build logs (on FAILED read the logs to explain why).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in buildGetIn) (*mcp.CallToolResult, buildGetOut, error) {
		reg, err := h.requireRegistry()
		if err != nil {
			return nil, buildGetOut{}, err
		}
		task := reg.GetBuild(in.ID)
		if task == nil {
			return nil, buildGetOut{}, fmt.Errorf("build %q not found (recent builds only; use build_list)", in.ID)
		}
		tail := in.LogsTail
		if tail <= 0 {
			tail = 40
		}
		if tail > 200 {
			tail = 200
		}
		logs := task.Logs
		if len(logs) > tail {
			logs = logs[len(logs)-tail:]
		}
		out := buildGetOut{
			ID: task.ID, Status: string(task.Status), Progress: task.Progress,
			Image: task.Image, Error: task.Error,
			LogsTail:   truncStr(joinLines(logs)),
			FinishedAt: formatTimePtr(task.FinishedAt),
		}
		if out.Status == string(registry.BsSuccess) {
			out.Hint = "deploy it as image " + task.Image + " (service_deploy config) — clusters pull it via the worker registry relay"
		}
		return nil, out, nil
	})

	// build_list
	mcp.AddTool(s, &mcp.Tool{
		Name:        "build_list",
		Description: "List recent image builds (newest first, without logs).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in buildListIn) (*mcp.CallToolResult, buildListOut, error) {
		reg, err := h.requireRegistry()
		if err != nil {
			return nil, buildListOut{}, err
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 20
		}
		tasks := reg.ListBuilds(limit)
		items := make([]buildOut, 0, len(tasks))
		for _, t := range tasks {
			items = append(items, buildOut{ID: t.ID, Status: string(t.Status), Image: t.Image})
		}
		return nil, buildListOut{Items: items}, nil
	})
}

// baseURL 直传端点的基础地址：从本次 MCP 请求的 Host 推断（助手既然能
// 到达 /mcp，同 host 的直传端点必然可达；TLS 终结场景尊重 X-Forwarded-Proto）。
func (h *Handler) baseURL(ctx context.Context) string {
	return baseURLFrom(ctx)
}

// formatTimePtr 指针时间格式化。
func formatTimePtr(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
