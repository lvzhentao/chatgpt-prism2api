# DESIGN.md — 视觉规范（替换此文件）

本文件是**风格入口**。内核里的管理台只是可跑的参考实现（羊皮纸 / Inter），
**不是**新站的最终外观。

换站时用一份站点 `DESIGN.md` 整文件替换本文件，然后把 token 落到：

| 落到 | 改什么 |
|---|---|
| `web/src/index.css` | 色板、字体、圆角、阴影、密度 |
| `web/index.html` | 字体 link / title |
| `web/src/lib/brand.ts` | 产品名、主色、favicon 字（身份，不是整套视觉） |
| `web/src/components/ui/*` | 按钮、输入、卡片是否跟规范一致 |
| 登录页 / 侧栏 / 页头文案语气 | 按 DESIGN 的语音，不留模板口吻 |

没有 DESIGN.md 时：先问，或按站点品牌写一版再落地。
**禁止**以「已经套了模板」为由跳过视觉。

## 最低要写清的块（给后续落地用）

- Theme：light / dark / both
- Colors：canvas、surface、text、border、accent、danger（含 hex）
- Typography：sans / serif / mono，字号与字重
- Radius / shadow / 间距密度
- 组件要点：主按钮、输入、侧栏、表格
- 语气：登录页和空状态怎么说话

对照范式里的长文风格说明书：`cursor2api/DESIGN.md`（只读，不要当新站默认）。
