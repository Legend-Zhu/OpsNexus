// 模型单价管理（MLOps P2，方案 §4.6/§6.2）：定点整数存储（微元/百万
// token），API 以最多 6 位小数的十进制字符串往返；保存即新版本快照，
// 明细入账时锁定当时单价，修改不回溯历史。写操作走 admin + 审计。
package mlops

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// PricingInput 保存单价入参（价格为「元/百万 token」十进制字符串）。
type PricingInput struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	PriceInPerM  string `json:"price_in_per_m"`
	PriceOutPerM string `json:"price_out_per_m"`
	Note         string `json:"note,omitempty"`
}

// ListPricings 列出全部单价。
func (s *Service) ListPricings() ([]*store.MLPricing, error) {
	return s.st.ListMLPricings()
}

// SavePricing 保存（覆盖）一个 (provider, model) 的单价。
func (s *Service) SavePricing(in PricingInput, operator string) (*store.MLPricing, error) {
	in.Provider = strings.TrimSpace(in.Provider)
	in.Model = strings.TrimSpace(in.Model)
	if in.Provider == "" || in.Model == "" {
		return nil, ErrInvalid{Msg: "provider and model are required"}
	}
	if len(in.Provider) > 128 || len(in.Model) > 128 {
		return nil, ErrInvalid{Msg: "provider/model too long (max 128)"}
	}
	if strings.ContainsAny(in.Provider, "\x00\r\n") || strings.ContainsAny(in.Model, "\x00\r\n") {
		return nil, ErrInvalid{Msg: "provider/model contains invalid characters"}
	}
	priceIn, err := parseMicroDecimal(in.PriceInPerM)
	if err != nil {
		return nil, ErrInvalid{Msg: "price_in_per_m: " + err.Error()}
	}
	priceOut, err := parseMicroDecimal(in.PriceOutPerM)
	if err != nil {
		return nil, ErrInvalid{Msg: "price_out_per_m: " + err.Error()}
	}

	before, _ := s.st.GetMLPricing(in.Provider, in.Model)
	now := time.Now().UTC()
	p := &store.MLPricing{
		Provider:          in.Provider,
		Model:             in.Model,
		Currency:          s.currency,
		PriceInPerMMicro:  priceIn,
		PriceOutPerMMicro: priceOut,
		Version:           fmt.Sprintf("pv-%d", now.UnixNano()),
		UpdatedAt:         now,
		UpdatedBy:         operator,
		Note:              strings.TrimSpace(in.Note),
	}
	if err := s.st.SaveMLPricing(p); err != nil {
		return nil, err
	}
	s.invalidatePricing(in.Provider, in.Model, p)
	bh := ""
	if before != nil {
		bh = pricingHash(before)
	}
	s.audit("pricing", operator, "pricing_save", in.Provider+"/"+in.Model, bh, pricingHash(p), "ok", "")
	return p, nil
}

// DeletePricing 删除单价（该模型后续调用 priced=false）。
func (s *Service) DeletePricing(provider, model, operator string) error {
	provider, model = strings.TrimSpace(provider), strings.TrimSpace(model)
	if provider == "" || model == "" {
		return ErrInvalid{Msg: "provider and model are required"}
	}
	before, err := s.st.GetMLPricing(provider, model)
	if err != nil {
		return err
	}
	if before == nil {
		return ErrNotFound{ID: "pricing " + provider + "/" + model}
	}
	if err := s.st.DeleteMLPricing(provider, model); err != nil {
		return err
	}
	s.invalidatePricing(provider, model, nil)
	s.audit("pricing", operator, "pricing_delete", provider+"/"+model, pricingHash(before), "", "ok", "")
	return nil
}

// pricingHash 单价记录摘要（审计留痕，不含敏感内容——单价本身非敏感，
// 但保持与其他对象一致的 hash 口径）。
func pricingHash(p *store.MLPricing) string {
	b := fmt.Appendf(nil, "%s|%s|%d|%d", p.Provider, p.Model, p.PriceInPerMMicro, p.PriceOutPerMMicro)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// parseMicroDecimal 十进制字符串 → 微元定点整数（×1e6）。
// 格式：1~10 位整数 + 可选小数（最多 6 位）；不接受符号/科学计数/超精度。
func parseMicroDecimal(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty price")
	}
	intPart, fracPart, hasDot := s, "", false
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart, hasDot = s[:i], s[i+1:], true
	}
	if intPart == "" || (hasDot && fracPart == "") || len(fracPart) > 6 {
		return 0, fmt.Errorf("invalid decimal %q (max 6 fraction digits)", s)
	}
	var v int64
	for _, c := range intPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid decimal %q", s)
		}
		v = v*10 + int64(c-'0')
		if v > 1_000_000_000 {
			return 0, fmt.Errorf("price %q too large", s)
		}
	}
	v *= 1_000_000
	for i := 0; i < 6; i++ {
		d := int64(0)
		if i < len(fracPart) {
			c := fracPart[i]
			if c < '0' || c > '9' {
				return 0, fmt.Errorf("invalid decimal %q", s)
			}
			d = int64(c - '0')
		}
		v += d * pow10int64(5-i)
	}
	return v, nil
}

func pow10int64(n int) int64 {
	v := int64(1)
	for i := 0; i < n; i++ {
		v *= 10
	}
	return v
}

// MicroToDecimal 微元定点 → 十进制字符串（去掉多余尾零），DTO 展示用。
func MicroToDecimal(v int64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	whole := v / 1_000_000
	frac := v % 1_000_000
	var out string
	if frac == 0 {
		out = strconv.FormatInt(whole, 10)
	} else {
		fs := fmt.Sprintf("%06d", frac)
		out = strconv.FormatInt(whole, 10) + "." + strings.TrimRight(fs, "0")
	}
	if neg {
		return "-" + out
	}
	return out
}
