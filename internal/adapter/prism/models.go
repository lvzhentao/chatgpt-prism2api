package prism

import "prism-2api/internal/adapter"

// ============================================================
// 定制点 2 — 模型目录。上游没有 /models 接口（HAR 全量端点表里没有），
// 目录来源是「账号自己的模型清单」：登录流程里 Statsig 的一次 /rgstr 响应
// gate "62892348" 的 value 直接给出该账号可用模型：
//
//	{"free_model":"gpt-5.6-terra","free_reasoning_effort":"high",
//	 "models":[{"id":"gpt-6-astra","label":"6 Astra"},
//	           {"id":"gpt-5.6-sol","label":"5.6 Sol"},
//	           {"id":"gpt-5.6-terra","label":"5.6 Terra"}]}
//
// 真机验证（YOUR_SERVER_IP）：gpt-6-astra / gpt-5.6-sol / gpt-5.6-terra 可用；
// 不在清单里的名字（gpt-5.4 / gpt-5.5 / gpt-5.6-luna）上游回
// 「Error while processing conversation (400 Bad Request)」。
// ============================================================

// DefaultModel 是对话默认模型；metadata.reasoning_effort 由请求的 thinking 档位决定。
//
// 2026-09-17 线上实测：gpt-6-astra 上游恒 400（crixet-backend
// /codex_v2_restore_start 400 Bad Request，多个账号复现），故默认落到 gpt-5.6-sol。
// astra 别名保留，等上游恢复后可切回。
const DefaultModel = "gpt-5.6-sol"

// thinkingLevels 是上游 metadata.reasoning_effort 的可用档位（HAR：medium/high，
// 账号清单 free_reasoning_effort=high）。内核统一的 thinking 变体后缀
// （-low/-medium/-high/-xhigh/-max，见 adapter.ParseModelID）会落到这三个值上。
//
// SupportsImages 对三个模型全开：多模态不是模型能力差异，而是适配器的项目文件通路
// （图片上传 → project_path 引用 → 模型自己读文件），三个模型走的是同一条路。
func staticModels() []*adapter.ModelInfo {
	return []*adapter.ModelInfo{
		{
			ID:                "gpt-6-astra",
			ServerModelName:   "gpt-6-astra",
			DisplayName:       "6 Astra",
			Aliases:           []string{"prism", "chatgpt", "astra"},
			SupportsThinking:  true,
			SupportsImages:    true,
			ContextTokenLimit: 0,
		},
		{
			ID:                "gpt-5.6-sol",
			ServerModelName:   "gpt-5.6-sol",
			DisplayName:       "5.6 Sol",
			Aliases:           []string{"sol", "default"},
			SupportsThinking:  true,
			SupportsImages:    true,
			ContextTokenLimit: 0,
		},
		{
			ID:                "gpt-5.6-terra",
			ServerModelName:   "gpt-5.6-terra",
			DisplayName:       "5.6 Terra",
			Aliases:           []string{"terra", "free"},
			SupportsThinking:  true,
			SupportsImages:    true,
			ContextTokenLimit: 0,
		},
	}
}
