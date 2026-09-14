# Quoin 品牌素材

原始用户素材保留在上一级，未覆盖。本目录是可用于网页接入的透明 PNG 和图标导出，不是矢量源文件。

## 文件用途

- `lockup-light.png`：浅色背景上的标志与 Quoin 字标组合。
- `lockup-dark.png`：深色背景上的浅色组合，保留蓝色强调。
- `mark-light.png` / `mark-dark.png`：浅色／深色背景用独立标志。
- `mark-black.png` / `mark-white.png`：纯黑／纯白单色标志。
- `illustration-core.png`：登录插画中央品牌层叠平台。
- `evidence-monitoring.png`：监控证据节点。
- `evidence-events.png`：事件线索节点。
- `evidence-knowledge.png`：知识节点。
- `investigation-record.png`：调查记录节点，没有虚构的完成状态。
- `app-icon-512.png` / `app-icon-light-512.png`：深浅底应用图标。
- `icon-16.png` / `icon-32.png` / `icon-48.png`：浏览器小图标。
- `icon-180.png`：Apple touch icon 尺寸。
- `icon-192.png`：应用图标常用尺寸。
- `favicon.ico`：包含 16、32、48 像素的浏览器图标。

五个插画元素独立导出，文字标题与连接线应在网页中绘制，以便保持清晰度、可调整布局及分段动画。不要把这批独立元素误认为已经拼好的登录插画。

## 来源与处理

- 用户提供 `独立品牌标志.png`、`品牌组合-浅色背景.png`、`品牌设计.png`。
- 深色组合及五个插画元素由配置端点上的 `gpt-image-2` 经已安装 `gpt-image` CLI 生成；每次一张、`quality=high`、PNG、参考编辑模式。
- 提示词位于 `docs/frontend/brand/prompts/`。生成时使用纯品红抠图底，再导出透明背景。蓝色／石墨色／白色主体保留；独立标志单色及深色版从已有符号派生，避免形态漂移。
- 请求尺寸：品牌组合 2304×768；核心平台及调查记录 1024×1024；三个来源节点 1536×512。端点实际输出尺寸可能不同，成品经去底、裁切、留边后以实际文件尺寸为准。
- 原始模型输出保存在本地 `.artifacts/brand-assets-20260914/`，该目录为开发产物；用户原始素材不变。
- 本目录没有承诺或伪造 Figma、AI、SVG 源文件，也不将设计展示图上的授权文字视为已核实的商业授权。

本轮完成素材生成与导出；随后素材已接入网页运行时：优化后的副本位于 `web/public/brand/`，用途与哈希记录在 `docs/frontend/brand/runtime-assets.yaml`。本目录仍是导出源头，保持不变。
