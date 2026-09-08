// 构建包直传：一次性票据 + 独立 HTTP 端点。zip 不走 MCP 协议（JSON 入参
// 装不下、也不该让助手上下文背着几十 MB 的 base64）。票据一次性、TTL 1h、
// 上传体大小必须与声明的 size_bytes 一致；直传端点只认票据，不暴露长效
// MCP token。
package mcpserver

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	ticketTTL = time.Hour
	// ticketBytes 票据随机字节数（hex 后 64 字符）。
	ticketBytes = 32
)

// ticketState 票据状态：pending（等直传）→ done（已落临时文件，等 build_submit）。
type ticketState int

const (
	ticketPending ticketState = iota
	ticketDone
)

// uploadTicket 一次构建包上传会话。
type uploadTicket struct {
	ID        string
	Ticket    string // 仅 pending 态有效；done 态清空
	State     ticketState
	Filename  string
	SizeBytes int64 // 声明大小 = 上传限额
	Path      string
	CreatedAt time.Time
}

// uploadTable 内存票据表（进程内、TTL 短，无需持久化；重启丢失 =
// 助手从 build_upload_begin 重来，代价可忽略）。
type uploadTable struct {
	mu      sync.Mutex
	tickets map[string]*uploadTicket
}

func newUploadTable() *uploadTable {
	return &uploadTable{tickets: map[string]*uploadTicket{}}
}

// begin 登记一次上传，返回票据。size 封顶 registry.max_upload_mb。
func (t *uploadTable) begin(filename string, size int64, maxUploadMB int) (*uploadTicket, error) {
	if maxUploadMB > 0 && size > int64(maxUploadMB)<<20 {
		return nil, fmt.Errorf("declared size %d bytes exceeds the %dMB upload limit", size, maxUploadMB)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked()
	id, err := randomHexID()
	if err != nil {
		return nil, err
	}
	ticket, err := randomHexID()
	if err != nil {
		return nil, err
	}
	rec := &uploadTicket{
		ID: id, Ticket: ticket, State: ticketPending,
		Filename: filename, SizeBytes: size,
		CreatedAt: time.Now(),
	}
	t.tickets[id] = rec
	return rec, nil
}

// take 取走已完成的临时文件（build_submit 消费，一次性）。
func (t *uploadTable) take(id string) (path, filename string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.tickets[id]
	if !ok {
		return "", "", fmt.Errorf("upload %q not found or already consumed", id)
	}
	if rec.State != ticketDone {
		return "", "", fmt.Errorf("upload %q has no file yet: run the curl command from build_upload_begin first", id)
	}
	if time.Since(rec.CreatedAt) > ticketTTL {
		delete(t.tickets, id)
		return "", "", fmt.Errorf("upload %q expired", id)
	}
	delete(t.tickets, id)
	return rec.Path, rec.Filename, nil
}

// upload 直传端点处理：校验票据（pending 态 + 匹配 + TTL）→ 按声明大小
// 落临时文件 → 置 done。消费的语义边界：pending 校验通过后即置为
// streaming（借 State 字段占位，防同一票据并发二次上传）。
func (h *Handler) UploadBuildPackage(c *gin.Context) {
	id := c.Query("upload_id")
	ticket := c.Query("ticket")

	t := h.tickets
	t.mu.Lock()
	rec, ok := t.tickets[id]
	if !ok || rec.State != ticketPending || rec.Ticket == "" || rec.Ticket != ticket {
		t.mu.Unlock()
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing/invalid/expired upload ticket"})
		return
	}
	if time.Since(rec.CreatedAt) > ticketTTL {
		delete(t.tickets, id)
		t.mu.Unlock()
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "upload ticket expired (restart from build_upload_begin)"})
		return
	}
	rec.Ticket = "" // 票据即刻作废（一次性）
	t.mu.Unlock()

	// 按声明大小写死上限：多一字节都拒绝（声明即限额）。
	tmp, err := os.CreateTemp("", "opsguard-mcp-upload-*.zip")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "create temp: " + err.Error()})
		return
	}
	written, err := io.Copy(tmp, io.LimitReader(c.Request.Body, rec.SizeBytes+1))
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(tmp.Name())
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "read upload: " + err.Error()})
		return
	}
	if written != rec.SizeBytes {
		os.Remove(tmp.Name())
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": fmt.Sprintf(
			"uploaded %d bytes but build_upload_begin declared %d; sizes must match exactly", written, rec.SizeBytes)})
		return
	}

	t.mu.Lock()
	rec.State = ticketDone
	rec.Path = tmp.Name()
	t.mu.Unlock()

	c.JSON(http.StatusOK, gin.H{"code": 200, "message": "ok", "data": gin.H{
		"upload_id": rec.ID, "bytes": written,
		"next": "build_submit{upload_id: \"" + rec.ID + "\", name: ..., tag: ...}",
	}})
}

// sweepLocked 清理过期票据（含已 done 未消费的临时文件）。
func (t *uploadTable) sweepLocked() {
	for id, rec := range t.tickets {
		if time.Since(rec.CreatedAt) > ticketTTL {
			if rec.State == ticketDone && rec.Path != "" {
				os.Remove(rec.Path)
			}
			delete(t.tickets, id)
		}
	}
}

// randomHexID 随机 hex id。
func randomHexID() (string, error) {
	b := make([]byte, ticketBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
